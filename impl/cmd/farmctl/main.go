// SPDX-License-Identifier: AGPL-3.0-only

// Command farmctl is the operator's tool for P2: it applies manifests, adds people and
// memberships, enrolls workers and checks the core. Every command goes over P2 with the
// operator credential of one tenant.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

const usage = `farmctl: the operator's tool for the P2 admin API

usage: farmctl [-p2 URL] [-token-file FILE] <command> [arguments]

commands:
  apply [-commit SHA] [-dry-run] [-as-test] [-drop-unsupported [-spec-root DIR]] PATH...
                                             apply the manifests of files and folders as one set
  proposal link-commit PROPOSAL SHA         link an applied commit to an accepted manifest change (PRP-19)
  principal add [-id ID] [-identity ID] [-locale L]
  principal off|on|offboard ID
  principal set-identity ID IDENTITY         for example telegram:123456
  grant ID GROUP [-until TIME]
  ungrant ID GROUP
  worker enroll -tenant T -role R -kind K [-target NAME] [-bot ID] -token-out FILE
                [-plugins P[,P] -placement host|sandbox [-host MACHINE]]   (toolhost)
  worker revoke ID                           the token stops working; the bot identity becomes unbound
  bot list | bot show ID | bot retire ID     bot identities of the credential's tenant (IDN-8, IDN-11)
  incident list | incident show|ack|close ID | incident dismiss ID REASON DETAILS
  control list [TENANT] | control revert incident=ID|since=TIME
  control apply OP KIND ID [incident=ID] [ttl=DURATION] [effects=E1,E2]
                                            incidents of the credential's tenant (MGR-8)
  seat assign SEAT BOT | seat vacate SEAT    a commission seat, for example commission:1 (IDN-10)
  seat assign farm-manager | seat vacate farm-manager | seat resume farm-manager [--incident] INCIDENT
                                         the farm manager seat, with the operator credential of farm (MGR-1, GOV-22)
  audit verify TENANT [-heads FILE]          FILE: the JSON array of heads from the monitoring history, oldest first
  audit query [-kind K] [-command C] [-actor P] [-target-kind K] [-from T] [-to T] [-limit N] [-after-seq N]
                                             the audit of the credential's tenant; the read is audited
  status [TENANT]                            the applied commit, holds and the conformance claim
  doctor [TENANT]                            farm checks: loop ticks, store volume, indicators, dead work
  dead [TENANT] [-after CURSOR]              dead tasks and deliveries
  redrive task|delivery ID                   new work from dead work
  spend reconcile -provider P -file FILE     hand in the provider's daily sums per workspace, one request
                                             per UTC day; FILE is CSV with the header day,tenant,workspace,eur
  drain [-until 30m]                         drain the core before a rollout; the next core start ends it
  backup report -kind backup|restore_check -location L -started T -finished T [-ok] [-size N]
  erasure-log export -config FILE            the erasure log as JSON for its separate backup (core machine)
  restore begin -config FILE -erasure-log FILE   mark a restored store before the core starts (core machine)
  restore complete [-spend-pending]          make the core ready after a restore
  hold set KIND [TENANT [NAME]] -effects E[,E] -reason R [-until TIME]
  hold release ID
  hold list [TENANT]
  hold farm -config FILE -effects E[,E] -reason R [-until TIME] [-by PRINCIPAL]
                                             a hold of scope farm written straight to the store
  health
  new-secret                                 print a random secret (for tokens and keys)
  hash-token                                 print the SHA-256 of a token read from stdin
  version
`

func main() {
	fs := flag.NewFlagSet("farmctl", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	p2 := fs.String("p2", envOr("BOTARU_P2", "http://127.0.0.1:8402"), "the P2 address of the core")
	tokenFile := fs.String("token-file", os.Getenv("BOTARU_TOKEN_FILE"), "the file with the operator credential")
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	c := &client{url: strings.TrimRight(*p2, "/"), tokenFile: *tokenFile}
	if err := run(c, args); err != nil {
		fmt.Fprintln(os.Stderr, "farmctl:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(c *client, args []string) error {
	switch args[0] {
	case "version":
		fmt.Println(version.String("farmctl"))
		return nil
	case "new-secret":
		fmt.Println(ids.Secret(32))
		return nil
	case "hash-token":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		h := sha256.Sum256([]byte(strings.TrimRight(string(b), "\r\n")))
		fmt.Println(hex.EncodeToString(h[:]))
		return nil
	case "health":
		return c.print("health", map[string]any{})
	case "apply":
		return apply(c, args[1:])
	case "proposal":
		if len(args) != 4 || args[1] != "link-commit" {
			return errors.New("usage: farmctl proposal link-commit PROPOSAL SHA")
		}
		return c.print("proposal.link_commit", map[string]any{"proposal": args[2], "commit": args[3]})
	case "principal":
		return principal(c, args[1:])
	case "grant", "ungrant":
		fs := flag.NewFlagSet(args[0], flag.ExitOnError)
		until := fs.String("until", "", "membership end, RFC 3339")
		_ = fs.Parse(reorder(args[1:]))
		if fs.NArg() != 2 {
			return errors.New(args[0] + " needs ID GROUP")
		}
		req := map[string]any{"op": args[0], "subject": fs.Arg(0), "group": fs.Arg(1)}
		if *until != "" {
			req["until"] = *until
		}
		return c.print(args[0], req)
	case "worker":
		if len(args) == 3 && args[1] == "revoke" {
			return c.print("worker.revoke", map[string]any{"op": "worker.revoke", "worker": args[2]})
		}
		if len(args) < 2 || args[1] != "enroll" {
			return errors.New("worker needs enroll or revoke")
		}
		return enroll(c, args[2:])
	case "bot":
		switch {
		case len(args) == 2 && args[1] == "list":
			return c.print("bot.list", map[string]any{})
		case len(args) == 3 && args[1] == "show":
			return c.print("bot.show", map[string]any{"op": "bot.show", "id": args[2]})
		case len(args) == 3 && args[1] == "retire":
			return c.print("bot.retire", map[string]any{"bot": args[2]})
		}
		return errors.New("usage: farmctl bot list | bot show ID | bot retire ID")
	case "control":
		switch {
		case len(args) == 2 && args[1] == "list":
			return c.print("control.list", map[string]any{})
		case len(args) == 3 && args[1] == "list":
			return c.print("control.list", map[string]any{"tenant": args[2]})
		case len(args) == 3 && args[1] == "revert":
			k, v, ok := strings.Cut(args[2], "=")
			if ok && (k == "incident" || k == "since") {
				return c.print("control.revert", map[string]any{k: v})
			}
		case len(args) >= 5 && args[1] == "apply":
			req := map[string]any{"op": args[2], "target": map[string]any{"kind": args[3], "id": args[4]}}
			for _, a := range args[5:] {
				k, v, _ := strings.Cut(a, "=")
				switch k {
				case "incident":
					req["basis"] = map[string]any{"incident": v}
				case "ttl":
					req["ttl"] = v
				case "effects":
					effects := []any{}
					for _, e := range strings.Split(v, ",") {
						effects = append(effects, e)
					}
					req["effects"] = effects
				default:
					return fmt.Errorf("unknown argument %s", a)
				}
			}
			return c.print("control.apply", req)
		}
		return errors.New("usage: farmctl control list [TENANT] | control revert incident=ID|since=TIME | control apply OP KIND ID [incident=ID] [ttl=DURATION] [effects=E1,E2]")
	case "incident":
		switch {
		case len(args) == 2 && args[1] == "list":
			return c.print("incident.list", map[string]any{})
		case len(args) == 3 && args[1] == "list":
			return c.print("incident.list", map[string]any{"tenant": args[2]})
		case len(args) == 3 && (args[1] == "show" || args[1] == "ack" || args[1] == "close"):
			return c.print("incident."+args[1], map[string]any{"op": "incident." + args[1], "id": args[2]})
		case len(args) >= 5 && args[1] == "dismiss":
			return c.print("incident.dismiss", map[string]any{"incident": args[2], "reason_code": args[3], "details": strings.Join(args[4:], " ")})
		}
		return errors.New("usage: farmctl incident list [TENANT] | incident show|ack|close ID | incident dismiss ID REASON DETAILS")
	case "seat":
		switch {
		case len(args) == 4 && args[1] == "assign":
			return c.print("seat.assign", map[string]any{"seat": args[2], "bot": args[3]})
		case len(args) == 3 && args[1] == "assign":
			return c.print("seat.assign", map[string]any{"seat": args[2]})
		case len(args) == 3 && args[1] == "vacate":
			return c.print("seat.vacate", map[string]any{"seat": args[2]})
		case len(args) == 4 && args[1] == "resume" && !strings.HasPrefix(args[3], "-"):
			return c.print("seat.resume", map[string]any{"seat": args[2], "incident": args[3]})
		case len(args) == 5 && args[1] == "resume" && args[3] == "--incident":
			return c.print("seat.resume", map[string]any{"seat": args[2], "incident": args[4]})
		}
		return errors.New("usage: farmctl seat assign SEAT [BOT] | seat vacate SEAT | seat resume farm-manager [--incident] INCIDENT")
	case "hold":
		return hold(c, args[1:])
	case "audit":
		if len(args) >= 2 && args[1] == "query" {
			return auditQuery(c, args[2:])
		}
		if len(args) < 3 || args[1] != "verify" {
			return errors.New("audit verify TENANT [-heads FILE], or audit query [flags]")
		}
		fs := flag.NewFlagSet("audit verify", flag.ExitOnError)
		headsFile := fs.String("heads", "", "a JSON array of the heads the monitoring history kept, oldest first (MON-3)")
		_ = fs.Parse(args[3:])
		req := map[string]any{"tenant": args[2]}
		if *headsFile != "" {
			b, err := os.ReadFile(*headsFile)
			if err != nil {
				return err
			}
			var heads []any
			if err := json.Unmarshal(b, &heads); err != nil {
				return fmt.Errorf("%s: %w", *headsFile, err)
			}
			req["heads"] = heads
		}
		return c.print("audit.verify", req)
	case "status", "doctor":
		req := map[string]any{}
		if len(args) == 2 {
			req["tenant"] = args[1]
		}
		return c.print(args[0], req)
	case "dead":
		fs := flag.NewFlagSet("dead", flag.ExitOnError)
		after := fs.String("after", "", "the cursor next of the previous answer")
		_ = fs.Parse(reorder(args[1:]))
		req := map[string]any{}
		if fs.NArg() == 1 {
			req["tenant"] = fs.Arg(0)
		}
		if *after != "" {
			req["after"] = *after
		}
		return c.print("deadletter.list", req)
	case "spend":
		if len(args) < 2 || args[1] != "reconcile" {
			return errors.New("spend needs reconcile")
		}
		return spendReconcile(c, args[2:])
	case "backup":
		if len(args) < 2 || args[1] != "report" {
			return errors.New("backup needs report")
		}
		return backupReport(c, args[2:])
	case "erasure-log":
		return erasureLog(args[1:])
	case "restore":
		return restore(c, args[1:])
	case "drain":
		fs := flag.NewFlagSet("drain", flag.ExitOnError)
		until := fs.String("until", "", "the drain deadline, for example 30m")
		_ = fs.Parse(args[1:])
		req := map[string]any{}
		if *until != "" {
			req["until"] = *until
		}
		return c.print("core.drain", req)
	case "redrive":
		if len(args) != 3 || (args[1] != "task" && args[1] != "delivery") {
			return errors.New("redrive task|delivery ID")
		}
		op := args[1] + ".redrive"
		return c.print(op, map[string]any{"op": op, "id": args[2]})
	}
	return fmt.Errorf("unknown command %q; run farmctl without arguments for help", args[0])
}

func auditQuery(c *client, args []string) error {
	fs := flag.NewFlagSet("audit query", flag.ExitOnError)
	str := map[string]*string{}
	for _, f := range []string{"kind", "command", "actor", "target-kind", "from", "to"} {
		str[f] = fs.String(f, "", "filter "+f)
	}
	limit := fs.Int("limit", 0, "at most this many records")
	afterSeq := fs.Int64("after-seq", 0, "records after this sequence number")
	_ = fs.Parse(args)
	req := map[string]any{}
	for f, v := range str {
		if *v != "" {
			req[strings.ReplaceAll(f, "-", "_")] = *v
		}
	}
	if *limit > 0 {
		req["limit"] = *limit
	}
	if *afterSeq > 0 {
		req["after_seq"] = *afterSeq
	}
	return c.print("audit.query", req)
}

// reorder puts flags before positional arguments, so that both orders work.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) && args[i] != "-dry-run" && args[i] != "-as-test" && args[i] != "-drop-unsupported" {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, args[i])
	}
	return append(flags, pos...)
}

func apply(c *client, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	commit := fs.String("commit", "", "the git commit of the manifests (default: HEAD of the first path)")
	dry := fs.Bool("dry-run", false, "plan only")
	asTest := fs.Bool("as-test", false, "apply every Tenant with kind: test, as the runner does on a test bench")
	drop := fs.Bool("drop-unsupported", false, "leave out the whole optional fields that the core refuses as of a later version or of a profile it does not claim, with a warning for each (MAN-11)")
	specRoot := fs.String("spec-root", "", "the skeleton checkout, for -drop-unsupported (default: $"+spec.EnvRoot+", or a parent of the working directory)")
	_ = fs.Parse(reorder(args))
	if fs.NArg() == 0 {
		return errors.New("apply needs at least one path")
	}
	dir := fs.Arg(0)
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		dir = filepath.Dir(dir)
	}
	if *commit == "" {
		out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
		if err != nil {
			msg := ""
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				msg = strings.TrimSpace(string(ee.Stderr))
			}
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("apply: no -commit, and git could not read the commit of %s: %s", dir, msg)
		}
		*commit = strings.TrimSpace(string(out))
	}
	docs, err := manifest.ReadFiles(fs.Args())
	if err != nil {
		return err
	}
	ms := make([]map[string]any, len(docs))
	for i, d := range docs {
		if spec, ok := d.Value["spec"].(map[string]any); ok && *asTest && d.Kind == "Tenant" {
			spec["kind"] = "test"
		}
		ms[i] = d.Value
	}
	if *drop {
		root, err := spec.FindRoot(*specRoot)
		if err != nil {
			return fmt.Errorf("apply -drop-unsupported reads the schemas to see what can be left out: %w", err)
		}
		sp, err := spec.Load(root)
		if err != nil {
			return err
		}
		if err := dropUnsupported(c, sp, *commit, ms, docs); err != nil {
			return err
		}
	}
	req := map[string]any{"commit": *commit, "manifests": ms, "dry_run": *dry}
	// The persona files the charters pin go with the set, by their hash (GOV-4).
	if ps := personaFiles(docs); len(ps) > 0 {
		req["personas"] = ps
	}
	// The commit message carries the Botaru-Proposal and Botaru-Revert lines (PRP-19, PRP-20).
	if out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%B", *commit).Output(); err == nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			req["message"] = msg
		}
	}
	return c.print("apply", req)
}

// dropUnsupported asks the core for a plan and removes every field that the plan refuses with
// version_unsupported. The core decides what it refuses; farmctl leaves those fields out and
// says so. Only a whole optional field is left out: a later kind, or a value that gives a field
// its meaning (a catalog resource kind, a channel direction, a tier), stops the apply with an
// error, because the manifest would then say something else.
func dropUnsupported(c *client, sp *spec.Spec, commit string, ms []map[string]any, docs []manifest.Doc) error {
	var plan struct {
		Errors []struct {
			Code string `json:"code"`
			Path string `json:"path"`
		} `json:"errors"`
	}
	if err := c.call("apply", map[string]any{"commit": commit, "manifests": ms, "dry_run": true}, &plan); err != nil {
		return err
	}
	type drop struct {
		i   int
		ptr string
	}
	var drops []drop
	for _, e := range plan.Errors {
		if e.Code != "version_unsupported" {
			continue
		}
		rest, ok := strings.CutPrefix(e.Path, "/manifests/")
		if !ok {
			continue
		}
		idx, ptr, _ := strings.Cut(rest, "/")
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(ms) {
			continue
		}
		ptr = "/" + ptr
		if err := sp.Droppable(ms[i], ptr); err != nil {
			return fmt.Errorf("%s: the core does not support %s, and it is not left out: %w", docs[i].Source, ptr, err)
		}
		drops = append(drops, drop{i, ptr})
	}
	for _, d := range drops {
		if !manifest.DropPointer(ms[d.i], d.ptr) {
			return fmt.Errorf("%s: the core does not support %s, and it cannot be left out", docs[d.i].Source, d.ptr)
		}
		fmt.Fprintf(os.Stderr, "warning: %s: left out %s (not supported by this core, MAN-11)\n", docs[d.i].Source, d.ptr)
	}
	return nil
}

func principal(c *client, args []string) error {
	if len(args) == 0 {
		return errors.New("principal needs add, off, on or set-identity")
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("principal add", flag.ExitOnError)
		id := fs.String("id", "", "the principal id (default: a new human:p_<ULID>)")
		identity := fs.String("identity", "", "an identity, for example telegram:123456")
		locale := fs.String("locale", "", "the person's locale, for example ru")
		_ = fs.Parse(reorder(args[1:]))
		if *id == "" {
			*id = ids.PrincipalID()
		}
		req := map[string]any{"op": "principal.add", "subject": *id}
		if *identity != "" {
			req["identity"] = *identity
		}
		if *locale != "" {
			req["locale"] = *locale
		}
		fmt.Println("principal:", *id)
		return c.print("principal.add", req)
	case "off", "on", "offboard":
		if len(args) != 2 {
			return errors.New("principal " + args[0] + " ID")
		}
		if args[0] == "offboard" {
			// The body of principal.offboard is the subject only (p2-principal-offboard-request).
			return c.print("principal.offboard", map[string]any{"subject": args[1]})
		}
		return c.print("principal."+args[0], map[string]any{"op": "principal." + args[0], "subject": args[1]})
	case "set-identity":
		if len(args) != 3 {
			return errors.New("principal set-identity ID IDENTITY")
		}
		return c.print("principal.set_identity", map[string]any{"op": "principal.set_identity", "subject": args[1], "identity": args[2]})
	}
	return fmt.Errorf("unknown principal command %q", args[0])
}

func enroll(c *client, args []string) error {
	fs := flag.NewFlagSet("worker enroll", flag.ExitOnError)
	tenant := fs.String("tenant", "", "the tenant")
	role := fs.String("role", "", "the role (for a channel or a toolhost: any name, for example the channel name)")
	kind := fs.String("kind", "", "agent, job, channel, sink, monitor or toolhost")
	target := fs.String("target", "", "the channel, sink or adapter name")
	plugins := fs.String("plugins", "", "toolhost: the plugins, comma separated, for example cmd,logs")
	placement := fs.String("placement", "", "toolhost: host or sandbox")
	host := fs.String("host", "", "toolhost of placement host: the catalog machine")
	bot := fs.String("bot", "", "agent or job: bind this unbound bot identity of the same role again")
	tokenOut := fs.String("token-out", "", "write the worker token to this file (mode 0600)")
	p2Out := fs.String("p2-token-out", "", "write the driver's P2 secret, if issued, to this file (default: the token file with .p2 added)")
	_ = fs.Parse(reorder(args))
	if *tenant == "" || *role == "" || *kind == "" || *tokenOut == "" {
		return errors.New("worker enroll needs -tenant, -role, -kind and -token-out")
	}
	req := map[string]any{"tenant": *tenant, "role": *role, "kind": *kind}
	if *target != "" {
		req["target"] = *target
	}
	if *plugins != "" {
		req["plugins"] = strings.Split(*plugins, ",")
	}
	if *placement != "" {
		req["placement"] = *placement
	}
	if *host != "" {
		req["host"] = *host
	}
	if *bot != "" {
		req["bot"] = *bot
	}
	var out struct {
		Worker  map[string]any `json:"worker"`
		Token   string         `json:"token"`
		P2Token string         `json:"p2_token"`
	}
	if err := c.call("worker.enroll", req, &out); err != nil {
		return err
	}
	if err := writeSecret(*tokenOut, out.Token); err != nil {
		return err
	}
	fmt.Println("worker:", out.Worker["id"], "token written to", *tokenOut)
	if out.P2Token != "" {
		if *p2Out == "" {
			*p2Out = *tokenOut + ".p2"
		}
		if err := writeSecret(*p2Out, out.P2Token); err != nil {
			return err
		}
		fmt.Println("driver P2 secret written to", *p2Out)
	}
	return nil
}

// writeSecret writes a token to a new file of mode 0600 and renames it into place, so an
// existing file with wider permissions never holds the token.
func writeSecret(path, secret string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type client struct {
	url, tokenFile string
}

func (c *client) call(op string, req, out any) error {
	token := ""
	if c.tokenFile != "" {
		t, err := config.Secret(c.tokenFile)
		if err != nil {
			return err
		}
		token = t
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/p2/"+op, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}

func (c *client) print(op string, req any) error {
	var out any
	if err := c.call(op, req, &out); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}

// personaFiles reads the persona files that the charters of a set pin: a ref is relative to the
// charter's folder or one of its parents. A file whose hash differs from the pin is still sent,
// and apply answers persona_missing for the pin.
func personaFiles(docs []manifest.Doc) map[string]string {
	out := map[string]string{}
	for _, d := range docs {
		if d.Kind != "Governance" {
			continue
		}
		spec, _ := d.Value["spec"].(map[string]any)
		for _, ref := range personaRefs(spec) {
			file, _, _ := strings.Cut(d.Source, "#")
			dir, _ := filepath.Abs(filepath.Dir(file))
			for {
				b, err := os.ReadFile(filepath.Join(dir, ref[0]))
				if err == nil {
					sum := sha256.Sum256(b)
					h := hex.EncodeToString(sum[:])
					if h != ref[1] {
						fmt.Fprintf(os.Stderr, "warning: %s: %s does not match its sha256 in the charter\n", d.Source, ref[0])
					}
					out[h] = string(b)
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					fmt.Fprintf(os.Stderr, "warning: %s: persona file %s is not found\n", d.Source, ref[0])
					break
				}
				dir = parent
			}
		}
	}
	return out
}

// personaRefRe is the path of a persona file as the charter schema allows it: no other file
// of the operator's machine is read.
var personaRefRe = regexp.MustCompile(`^personas/[a-z][a-z0-9-]*\.md$`)

// personaRefs are the pinned persona references of a charter spec: ref and sha256.
func personaRefs(spec map[string]any) [][2]string {
	var out [][2]string
	add := func(v any) {
		m, _ := v.(map[string]any)
		p, _ := m["persona"].(map[string]any)
		ref, _ := p["ref"].(string)
		sha, _ := p["sha256"].(string)
		if personaRefRe.MatchString(ref) && len(sha) == 64 {
			out = append(out, [2]string{ref, sha})
		}
	}
	pos, _ := spec["positions"].(map[string]any)
	for _, v := range pos {
		add(v)
	}
	com, _ := spec["commission"].(map[string]any)
	lenses, _ := com["lenses"].(map[string]any)
	for _, v := range lenses {
		add(v)
	}
	add(spec["farm_manager"])
	return out
}
