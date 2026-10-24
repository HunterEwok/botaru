// SPDX-License-Identifier: AGPL-3.0-only

// Package ingress is the E1 source driver webhook (spec/bindings/channels/webhook.md): an HTTP
// listener for the deliveries of one external system. It checks each delivery in the order of
// the binding (path, request rate, size, authenticity, source rate, body), saves the whole body
// as a file on behalf of the service principal hook:<source> and submits one signal of
// ingress.event with only the mapped fields of closed kinds (ING-1 to ING-4). A refused delivery
// gives no activity, no file and no event, and its answer carries no detail beyond the status.
package ingress

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// Core is what the driver calls of the core: P1 hello, file.put and submit_activity.
type Core interface {
	Call(ctx context.Context, op, lease string, req, out any) error
	FilePut(ctx context.Context, headers map[string]string, meta map[string]any, content []byte) (string, error)
}

// Format is a value format of spec/model/formats.yaml: its pattern and length ceiling.
type Format struct {
	Pattern   *regexp.Regexp
	MaxLength int
}

// Config is the settings of one source.
type Config struct {
	Core Core
	// Channel is the farm name of the source channel, and Spec its manifest spec.
	Channel string
	Spec    manifest.Channel
	// Secrets are the signing keys of auth: signature (the base64-decoded part of whsec_...);
	// two during a rotation.
	Secrets [][]byte
	// TokenHashes are the SHA-256 of the tokens of auth: tls_token; the driver keeps no token.
	TokenHashes [][sha256.Size]byte
	// ListenerPerMinute is the ceiling on refused requests (size or authenticity) from one
	// network in a sliding minute; 0 is ten times ingress.limits.per_minute.
	ListenerPerMinute int
	// Formats check the x-botaru-format leaves of the mapping; a format missing here passes no
	// value.
	Formats map[string]Format
	Now     func() time.Time
	Logger  *slog.Logger
}

// Driver serves the deliveries of one source.
type Driver struct {
	cfg    Config
	ing    *manifest.Ingress
	source string
	log    *slog.Logger

	mu sync.Mutex
	// refused holds the refused requests of each network, for the listener ceiling.
	refused  map[netip.Prefix][]time.Time
	accepted []time.Time // authentic deliveries, for the source's per_minute
	// done remembers the latest delivery ids the core took, so a repeat is answered at once
	// without a second file.
	done     map[string]bool
	doneList []string
	// inflight holds the delivery ids being passed to the core now, so two identical
	// deliveries at once do not both put a body file.
	inflight map[string]bool
}

// doneMax is the number of delivery ids the driver remembers; older repeats reach the core,
// which gives them no second event (ING-3).
const doneMax = 4096

// New checks the settings and returns a driver.
func New(cfg Config) (*Driver, error) {
	ing := cfg.Spec.Ingress
	if cfg.Spec.Direction != "in" || ing == nil || ing.Source() == "" {
		return nil, errors.New("the channel is not a source channel with an ingress block")
	}
	switch ing.Auth {
	case "signature":
		if len(cfg.Secrets) == 0 {
			return nil, errors.New("auth: signature needs a signing secret")
		}
		if d, ok := parseDuration(ing.Tolerance); !ok || d > 5*time.Minute {
			return nil, errors.New("auth: signature needs ingress.tolerance of at most 5m")
		}
	case "tls_token":
		if len(cfg.TokenHashes) == 0 {
			return nil, errors.New("auth: tls_token needs a token")
		}
	default:
		return nil, fmt.Errorf("unknown ingress.auth %q", ing.Auth)
	}
	if ing.Limits.PerMinute < 1 || ing.Limits.MaxBodyBytes < 1 {
		return nil, errors.New("ingress.limits needs per_minute and max_body_bytes")
	}
	if cfg.ListenerPerMinute == 0 {
		cfg.ListenerPerMinute = 10 * ing.Limits.PerMinute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Driver{cfg: cfg, ing: ing, source: ing.Source(), log: cfg.Logger, done: map[string]bool{}, inflight: map[string]bool{}, refused: map[netip.Prefix][]time.Time{}}, nil
}

// Path is the path of the source: /hooks/<source>.
func (d *Driver) Path() string { return "/hooks/" + d.source }

// Hello reports the source channel to the core with the declarations of its manifest.
func (d *Driver) Hello(ctx context.Context) error {
	return d.cfg.Core.Call(ctx, "hello", "", map[string]any{
		"kind": "channel", "implementation": "botaru-ingress " + version.Version, "capacity": 1, "protocol_versions": []string{"v1"},
		"driver": map[string]any{"ai_disclosure": d.cfg.Spec.AIDisclosure, "provenance": d.cfg.Spec.Provenance, "stores_content": "none"},
	}, nil)
}

// ServeHTTP checks one delivery and passes it to the core.
func (d *Driver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Method and path.
	if r.Method != http.MethodPost || r.URL.Path != d.Path() {
		http.NotFound(w, r)
		return
	}
	now := d.cfg.Now()
	// 2. The refused requests of the sender's network, before the body is read: only refusals
	// count, so a flood of forged requests from elsewhere never holds back an authentic one.
	src := network(r.RemoteAddr)
	if wait := d.refusedWait(src, now); wait > 0 {
		tooMany(w, wait)
		return
	}
	// 3. Size: refused whole, never truncated (ING-3).
	limit := d.ing.Limits.MaxBodyBytes
	if r.ContentLength > limit {
		d.refuse(src, now)
		status(w, http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		d.refuse(src, now)
		status(w, http.StatusBadRequest)
		return
	}
	if int64(len(body)) > limit {
		d.refuse(src, now)
		status(w, http.StatusRequestEntityTooLarge)
		return
	}
	// 4. Authenticity (ING-1).
	id, ok := d.authentic(r, body, now)
	if !ok {
		d.refuse(src, now)
		status(w, http.StatusUnauthorized)
		return
	}
	// 5. The source's own rate counts only authentic deliveries (ING-3); a repeat of a delivery
	// the core already took is answered without counting, so a replayed one uses up nothing.
	if d.isDone(id) {
		status(w, http.StatusNoContent)
		return
	}
	if wait := d.take(&d.accepted, now, d.ing.Limits.PerMinute); wait > 0 {
		tooMany(w, wait)
		return
	}
	// 6. The body is strict I-JSON.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		status(w, http.StatusUnsupportedMediaType)
		return
	}
	doc, err := ijson.Parse(body)
	if err != nil {
		status(w, http.StatusBadRequest)
		return
	}
	// 7. The delivery id is claimed before the body is put: a remembered one is answered at
	// once, and one in flight is asked again later, since the first may still fail (ING-3).
	switch d.claim(id) {
	case claimDone:
		status(w, http.StatusNoContent)
		return
	case claimBusy:
		w.Header().Set("Retry-After", "5")
		status(w, http.StatusServiceUnavailable)
		return
	}
	defer d.release(id)
	switch err := d.submit(r.Context(), id, doc, body, now); {
	case err == nil:
		d.remember(id)
		status(w, http.StatusNoContent)
	case retryable(err):
		w.Header().Set("Retry-After", "5")
		status(w, http.StatusServiceUnavailable)
	default:
		d.log.Warn("the core refused a delivery", "source", d.source, "reason", p1client.Reason(err))
		status(w, http.StatusUnprocessableEntity)
	}
}

// authentic checks a delivery by ingress.auth and returns its delivery id (ING-1, ING-3).
func (d *Driver) authentic(r *http.Request, body []byte, now time.Time) (string, bool) {
	sum := sha256.Sum256(body)
	bodyID := hex.EncodeToString(sum[:])
	switch d.ing.Auth {
	case "signature":
		id, ts, sigs := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"), r.Header.Get("webhook-signature")
		if id == "" || ts == "" || sigs == "" {
			return "", false
		}
		sec, err := strconv.ParseInt(ts, 10, 64)
		tol, _ := parseDuration(d.ing.Tolerance)
		if err != nil || math.Abs(now.Sub(time.Unix(sec, 0)).Seconds()) > tol.Seconds() {
			return "", false
		}
		signed := append([]byte(id+"."+ts+"."), body...)
		ok := false
		for _, key := range d.cfg.Secrets {
			mac := hmac.New(sha256.New, key)
			mac.Write(signed)
			want := []byte(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			for _, entry := range strings.Fields(sigs) {
				ver, sig, found := strings.Cut(entry, ",")
				if found && ver == "v1" && hmac.Equal([]byte(sig), want) {
					ok = true
				}
			}
		}
		if !ok {
			return "", false
		}
		// The signed id is the delivery id when it fits update_id; else the body hash.
		if !reUpdateID.MatchString(id) {
			id = bodyID
		}
		return id, true
	case "tls_token":
		auth := r.Header.Get("Authorization")
		token, found := strings.CutPrefix(auth, "Bearer ")
		if r.TLS == nil || !found || token == "" {
			return "", false
		}
		h := sha256.Sum256([]byte(token))
		ok := false
		for _, want := range d.cfg.TokenHashes {
			if subtle.ConstantTimeCompare(h[:], want[:]) == 1 {
				ok = true
			}
		}
		// A token source has no signed id: the delivery id is always the body hash.
		return bodyID, ok
	}
	return "", false
}

// reUpdateID is the form of update_id in spec/model/activity.schema.json.
var reUpdateID = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,128}$`)

// submit saves the body as a file on behalf of hook:<source> and submits the signal.
func (d *Driver) submit(ctx context.Context, id string, doc any, body []byte, now time.Time) error {
	hook := "hook:" + d.source
	ref, err := d.cfg.Core.FilePut(ctx, map[string]string{"Botaru-On-Behalf": hook, "Botaru-Chat": "direct:" + d.source},
		map[string]any{"media_type": "application/json", "filename": "delivery.json"}, body)
	if err != nil {
		return err
	}
	act := map[string]any{
		"kind": "signal", "channel": d.cfg.Channel, "update_id": id,
		"sender":      map[string]any{"identity": hook, "verified": true},
		"chat":        map[string]any{"kind": "direct", "external_id": d.source},
		"event":       map[string]any{"type": d.ing.Event, "data": d.mapped(doc)},
		"attachments": []string{ref},
		"received_at": now.UTC().Format(time.RFC3339),
	}
	return d.cfg.Core.Call(ctx, "submit_activity", "", act, nil)
}

// mapped reads each pointer of ingress.mapping and keeps the value only when it passes its
// closed schema; anything else stays only in the body file (ING-2).
func (d *Driver) mapped(doc any) map[string]any {
	out := map[string]any{}
	for _, name := range slices.Sorted(mapKeys(d.ing.Mapping)) {
		f := d.ing.Mapping[name]
		v, ok := pointer(doc, f.From)
		if ok && d.closedOK(f.Schema, v) {
			out[name] = v
		}
	}
	return out
}

// pointer resolves a JSON pointer of the mapping form (/a/b/0) in a parsed body.
func pointer(doc any, ptr string) (any, bool) {
	cur := doc
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		switch x := cur.(type) {
		case map[string]any:
			v, ok := x[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// reCatalogID is the form a catalog id of an enum_from leaf may take here; the core checks the
// value against the catalog list itself.
var reCatalogID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,119}$`)

// closedOK checks a value against a closed leaf of INJ-1: an enum or a const, a number or
// integer within its bounds, a boolean, a string of a format, a catalog id of an enum_from list,
// or a list of such. Free text never passes.
func (d *Driver) closedOK(s map[string]any, v any) bool {
	if e, ok := s["enum"].([]any); ok {
		return slices.ContainsFunc(e, func(x any) bool { return sameScalar(x, v) })
	}
	if c, ok := s["const"]; ok {
		return sameScalar(c, v)
	}
	if _, ok := s["enum_from"]; ok {
		str, ok := v.(string)
		return ok && reCatalogID.MatchString(str)
	}
	typ, _ := s["type"].(string)
	switch typ {
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer", "number":
		n, ok := number(v)
		if !ok || typ == "integer" && n != math.Trunc(n) {
			return false
		}
		if m, ok := number(s["minimum"]); ok && n < m {
			return false
		}
		if m, ok := number(s["maximum"]); ok && n > m {
			return false
		}
		return true
	case "string":
		str, ok := v.(string)
		name, _ := s["x-botaru-format"].(string)
		f, known := d.cfg.Formats[name]
		return ok && known && f.Pattern != nil && (f.MaxLength == 0 || len(str) <= f.MaxLength) && f.Pattern.MatchString(str)
	case "array":
		list, ok := v.([]any)
		items, _ := s["items"].(map[string]any)
		if !ok || items == nil {
			return false
		}
		if m, ok := number(s["maxItems"]); ok && float64(len(list)) > m {
			return false
		}
		for _, x := range list {
			if !d.closedOK(items, x) {
				return false
			}
		}
		return true
	}
	return false
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func sameScalar(a, b any) bool {
	na, okA := number(a)
	nb, okB := number(b)
	if okA || okB {
		return okA && okB && na == nb
	}
	switch a.(type) {
	case string, bool:
		return a == b
	}
	return false
}

// take counts one event in a sliding minute and returns how long to wait when the ceiling is
// reached (0: taken).
func (d *Driver) take(list *[]time.Time, now time.Time, ceiling int) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	from := now.Add(-time.Minute)
	kept := (*list)[:0]
	for _, t := range *list {
		if t.After(from) {
			kept = append(kept, t)
		}
	}
	*list = kept
	if len(kept) >= ceiling {
		return kept[0].Add(time.Minute).Sub(now)
	}
	*list = append(*list, now)
	return 0
}

// maxNetworks bounds the networks the listener ceiling remembers; past it, the networks
// without a refusal in the last minute are forgotten, and then all others share one count.
const maxNetworks = 4096

// network is the network of a remote address for the listener ceiling: a /24 for IPv4 and a
// /48 for IPv6, so that one sender cannot dodge the ceiling by changing the last bits.
func network(remote string) netip.Prefix {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Prefix{}
	}
	a = a.Unmap()
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, _ := a.Prefix(bits)
	return p
}

// refusedWait says how long a network waits once its refused requests reached the ceiling.
func (d *Driver) refusedWait(n netip.Prefix, now time.Time) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.refused[n]; !ok && len(d.refused) >= maxNetworks {
		// A network the full map does not hold shares the one count of the others.
		n = netip.Prefix{}
	}
	list := recent(d.refused[n], now)
	if len(list) == 0 {
		delete(d.refused, n)
		return 0
	}
	d.refused[n] = list
	if len(list) >= d.cfg.ListenerPerMinute {
		return list[0].Add(time.Minute).Sub(now)
	}
	return 0
}

// refuse counts a refused request of a network.
func (d *Driver) refuse(n netip.Prefix, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.refused[n]; !ok && len(d.refused) >= maxNetworks {
		for k, v := range d.refused {
			if len(recent(v, now)) == 0 {
				delete(d.refused, k)
			}
		}
		if len(d.refused) >= maxNetworks {
			n = netip.Prefix{}
		}
	}
	d.refused[n] = append(recent(d.refused[n], now), now)
}

// recent keeps the times of the last minute.
func recent(list []time.Time, now time.Time) []time.Time {
	from := now.Add(-time.Minute)
	kept := list[:0]
	for _, t := range list {
		if t.After(from) {
			kept = append(kept, t)
		}
	}
	return kept
}

// isDone says whether the core already took a delivery id.
func (d *Driver) isDone(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done[id]
}

const (
	claimNew = iota
	claimDone
	claimBusy
)

// claim takes a delivery id for this request: claimDone when the core already took it,
// claimBusy when another request passes it now.
func (d *Driver) claim(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.done[id]:
		return claimDone
	case d.inflight[id]:
		return claimBusy
	}
	d.inflight[id] = true
	return claimNew
}

// release ends the claim of a request.
func (d *Driver) release(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, id)
}

func (d *Driver) remember(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done[id] {
		return
	}
	d.done[id] = true
	d.doneList = append(d.doneList, id)
	if len(d.doneList) > doneMax {
		delete(d.done, d.doneList[0])
		d.doneList = d.doneList[1:]
	}
}

// retryable says whether the sender should deliver again: the core is not ready or not reached.
func retryable(err error) bool {
	var pe *p1client.Error
	if !errors.As(err, &pe) {
		return true
	}
	return pe.Reason == "not_ready" || pe.Status >= 500
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	status(w, http.StatusTooManyRequests)
}

// status answers with a status and no detail (webhook.md).
func status(w http.ResponseWriter, code int) { w.WriteHeader(code) }

// parseDuration reads the duration form of the schemas: 15m, 60s, 2h.
func parseDuration(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[s[len(s)-1]]
	n, err := strconv.Atoi(s[:len(s)-1])
	if unit == 0 || err != nil || n <= 0 {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
