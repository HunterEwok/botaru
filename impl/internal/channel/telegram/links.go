// SPDX-License-Identifier: AGPL-3.0-only

package telegram

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A link is a span of text that a Telegram client opens (INJ-11). The Bot API marks the links
// of a sent text by the rules of Telegram's own parser, so findLinks follows those rules: a
// host is found around each dot, takes a port, a path and the user part before an @, a scheme
// in front of it, and is a link when its last label is a top-level domain (any one with a
// scheme or a user part, a domain of webTLDs without one, any xn-- one) or it is an IPv4
// address. A mention (@name) opens a chat the same way.
type link struct {
	from, to int
	scheme   string // lower case, "" for none
	host     string // lower case
	user     bool   // a user part before an @
	ipv4     bool
}

// Verdicts of a span.
const (
	keep = iota
	defang
	cut
)

// schemeRE is a safety net over findLinks: any word with ://, and the schemes a client opens
// without //, are cut whatever their host.
var schemeRE = regexp.MustCompile(`(?i)[^\s<>"\x{ab}\x{bb}]*[a-z0-9]://[^\s<>"\x{ab}\x{bb}]*` +
	`|(?:^|[^\p{L}\p{N}_])((?:tg|ton|tonsite|mailto|tel|sms|callto|javascript|vbscript|data|file|magnet|intent|viber|whatsapp|skype):[^\s<>"\x{ab}\x{bb}]+)`)

// cleanText does what Telegram does to a text before it looks for links: it drops \r, the line
// and paragraph separators and the bidi marks U+2028 to U+202E and three combining lines, and
// turns the other control characters but \n into spaces, here the C1 ones as well, which
// Telegram keeps. The driver sends the cleaned text, so a link the cleaning would join is
// found here.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\r', r >= 0x2028 && r <= 0x202e, r == 0x030a, r == 0x0333, r == 0x033f:
			return -1
		case r < 0x20 && r != '\n', r >= 0x7f && r <= 0x9f:
			return ' '
		}
		return r
	}, s)
}

// cutLinks keeps the links of the allowlist, cuts a link with a scheme or a user part to
// "[link removed]", and defangs every other link and mention: its dots become U+2024 and its @
// U+FF20, so it reads the same and opens nothing. A pass can join what is left into a new
// link, so passes repeat until nothing changes.
func (c *Channel) cutLinks(s string) string {
	s = cleanText(s)
	for range 8 {
		next := c.cutPass(s)
		if next == s {
			return s
		}
		s = next
	}
	return "[link removed]"
}

func (c *Channel) cutPass(s string) string {
	type span struct{ from, to, verdict int }
	var spans []span
	for _, l := range findLinks(s) {
		spans = append(spans, span{l.from, l.to, c.verdict(l)})
	}
	for _, m := range findMentions(s) {
		spans = append(spans, span{m[0], m[1], defang})
	}
	for _, m := range schemeRE.FindAllStringSubmatchIndex(s, -1) {
		from, to := m[0], m[1]
		if m[2] >= 0 {
			from, to = m[2], m[3]
		}
		to = from + len(strings.TrimRight(s[from:to], ".,;:!?'`)"))
		same := false
		for _, sp := range spans {
			same = same || (sp.from == from && sp.to == to)
		}
		if !same && to > from {
			spans = append(spans, span{from, to, cut})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
	var out strings.Builder
	last := 0
	for i := 0; i < len(spans); {
		// Spans that overlap make one group; the strongest verdict in it applies to all of it.
		from, to, verdict := spans[i].from, spans[i].to, spans[i].verdict
		j := i + 1
		for ; j < len(spans) && spans[j].from < to; j++ {
			to = max(to, spans[j].to)
			verdict = max(verdict, spans[j].verdict)
		}
		out.WriteString(s[last:from])
		switch verdict {
		case keep:
			out.WriteString(s[from:to])
		case defang:
			out.WriteString(strings.NewReplacer(".", "\u2024", "@", "\uff20").Replace(s[from:to]))
		default:
			out.WriteString("[link removed]")
		}
		last, i = to, j
	}
	out.WriteString(s[last:])
	return out.String()
}

// verdict keeps an http link or a bare host of the tenant's list, cuts a link with another
// scheme or with a user part (an address, or a host hidden behind one), and defangs the rest.
func (c *Channel) verdict(l link) int {
	if l.scheme != "" && l.scheme != "http" && l.scheme != "https" || l.user {
		return cut
	}
	for _, a := range c.cfg.LinkAllowlist {
		if strings.EqualFold(l.host, a) && isASCII(l.host) {
			return keep
		}
	}
	if l.scheme != "" {
		return cut
	}
	return defang
}

// findMentions returns the spans of @name that Telegram can open: 2 to 32 letters, digits or _
// after an @, also after a word character, which a #tag@channel or $TAG@channel opens.
func findMentions(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); i++ {
		if s[i] != '@' {
			continue
		}
		j := i + 1
		for j < len(s) && (isAlnum(rune(s[j])) || s[j] == '_') {
			j++
		}
		if r, _ := utf8.DecodeRuneInString(s[j:]); j-i-1 < 2 || j-i-1 > 32 || j < len(s) && isWordRune(r) {
			continue
		}
		out = append(out, [2]int{i, j})
		i = j - 1
	}
	return out
}

// isSeparator is Telegram's separator class: a C1 control is not one there, but cleanText turns
// the controls into spaces.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func isSeparator(r rune) bool { return unicode.In(r, unicode.Z) }

func isURLRune(r rune) bool {
	if r >= 0x2000 && r <= 0x206f {
		return r == 0x200c || r == 0x200d || (r >= 0x2010 && r <= 0x2015)
	}
	return !isSeparator(r)
}

func isPathRune(r rune) bool {
	return !strings.ContainsRune("\n<>\"\u00ab\u00bb", r) && isURLRune(r)
}

func isAlnum(r rune) bool { return r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

func isDomainRune(r rune) bool {
	if r < 0xc0 {
		return isAlnum(r) || strings.ContainsRune("._-~", r)
	}
	return isURLRune(r)
}

func isUserRune(r rune) bool {
	return !strings.ContainsRune("\n/[]{}()'`<>\"@\u00ab\u00bb", r) && isURLRune(r)
}

func isProtocolRune(r rune) bool {
	if r < 0x80 {
		return isAlnum(r) || r == '+' || r == '-'
	}
	return !isSeparator(r)
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

// back walks left from i, not past floor, while ok holds and returns where the run starts.
func back(s string, floor, i int, ok func(rune) bool) int {
	for i > floor {
		r, n := utf8.DecodeLastRuneInString(s[:i])
		if !ok(r) {
			break
		}
		i -= n
	}
	return i
}

// forward walks right from i while ok holds and returns where the run ends.
func forward(s string, i int, ok func(rune) bool) int {
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !ok(r) {
			break
		}
		i += n
	}
	return i
}

const badPathEnd = ".:;,('?!`"

// findLinks returns the links of s in order.
func findLinks(s string) []link {
	var links []link
	for pos := 0; ; {
		d := strings.IndexByte(s[pos:], '.')
		if d < 0 || pos+d+1 >= len(s) {
			break
		}
		dot := pos + d
		if s[dot+1] == ' ' {
			pos = dot + 2
			continue
		}
		begin := back(s, pos, dot, isDomainRune)
		end, at := dot, -1
		for end < len(s) {
			r, n := utf8.DecodeRuneInString(s[end:])
			if r == '@' {
				at = end
			} else if !isDomainRune(r) {
				break
			}
			end += n
		}
		if at >= 0 {
			begin = back(s, pos, begin, isUserRune)
		}
		hostEnd := end
		if end < len(s) && s[end] == ':' {
			p := forward(s, end+1, func(r rune) bool { return r >= '0' && r <= '9' })
			digits := strings.TrimLeft(s[end+1:p], "0")
			if v, err := strconv.Atoi(digits); err == nil && len(digits) <= 5 && v <= 65535 {
				end = p
			}
		}
		pathFrom := end
		if end < len(s) && strings.IndexByte("/?#", s[end]) >= 0 {
			p := forward(s, end+1, isPathRune)
			for p > end+1 && strings.IndexByte(badPathEnd, s[p-1]) >= 0 {
				p--
			}
			if s[end] == '/' || p > end+1 {
				end = p
			}
		}
		for end > dot+1 && s[end-1] == '.' {
			end--
		}
		bad, user := false, false
		from := begin
		if from > pos && s[from-1] == '@' {
			user = true
			bad = bad || at >= 0
			u := back(s, pos, from-1, isUserRune)
			bad = bad || u == from-1
			from = u
		}
		scheme := ""
		if from > pos {
			// Telegram looks back only to the end of the previous link.
			if prefix := s[pos:from]; len(prefix) >= 6 && strings.HasSuffix(prefix, "://") {
				p := back(s, pos, from-3, isProtocolRune)
				proto := strings.ToLower(s[p : from-3])
				switch {
				case strings.HasSuffix(proto, "https"):
					scheme = "https"
				case strings.HasSuffix(proto, "http") && proto != "shttp":
					scheme = "http"
				case strings.HasSuffix(proto, "ftp") && proto != "tftp" && proto != "sftp":
					scheme = "ftp"
				case strings.HasSuffix(proto, "tonsite"):
					scheme = "tonsite"
				default:
					bad = true
				}
				if scheme != "" {
					from -= len(scheme) + 3
				}
			} else if r, _ := utf8.DecodeLastRuneInString(prefix); isWordRune(r) || r == '/' || r == '#' || r == '@' {
				bad = true
			}
		}
		if !bad {
			if end > dot+1 {
				l := link{from: from, scheme: scheme, user: user}
				if to, ok := checkLink(s, &l, s[begin:min(hostEnd, end)], min(pathFrom, end), end); ok {
					l.to = to
					links = append(links, l)
				}
			}
			for end < len(s) && s[end] == '.' {
				end++
			}
		} else {
			for end > dot+1 && s[end-1] != '.' {
				end--
			}
		}
		pos = max(end, dot+1)
	}
	return links
}

// checkLink decides whether a found host is a link: it must have two labels or more, each short
// and not ending in -, and end on a top-level domain or be an IPv4 address. The path stops at a
// bracket without its pair and before trailing punctuation; checkLink returns the link's end.
func checkLink(s string, l *link, domain string, pathFrom, end int) (int, bool) {
	if i := strings.IndexByte(domain, '@'); i >= 0 {
		l.user = true
		domain = domain[i+1:]
	}
	var depth [3]int
	n := pathFrom
	for ; n < end; n++ {
		if i := strings.IndexByte("([{", s[n]); i >= 0 {
			depth[i]++
		} else if i := strings.IndexByte(")]}", s[n]); i >= 0 {
			depth[i]--
			if depth[i] < 0 {
				break
			}
		}
	}
	for n > pathFrom && strings.IndexByte(badPathEnd, s[n-1]) >= 0 {
		n--
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return n, false
	}
	ipv4 := len(labels) == 4
	for _, p := range labels {
		if p == "" || len(p) >= 64 || p[len(p)-1] == '-' {
			return n, false
		}
		v, err := strconv.Atoi(p)
		if err != nil || strings.Trim(p, "0123456789") != "" || len(p) > 3 || v > 255 || (p[0] == '0' && len(p) > 1) {
			ipv4 = false
		}
	}
	// Only ASCII letters fold: Go folds U+0130 to i, a browser does not, so a look-alike host
	// must not match the allowlist.
	l.host = strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, domain)
	if ipv4 {
		l.ipv4 = true
		return n, true
	}
	if strings.Trim(domain, "0123456789.") == "" {
		return n, false
	}
	tld := strings.ToLower(labels[len(labels)-1])
	if utf8.RuneCountInString(tld) <= 1 {
		return n, false
	}
	if strings.HasPrefix(tld, "xn--") {
		if len(tld) <= 5 || strings.Trim(tld[4:], "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
			return n, false
		}
	} else if strings.ContainsAny(tld, "_-") || (l.scheme == "" && !l.user && !webTLDs[tld]) {
		return n, false
	}
	if strings.Contains(labels[len(labels)-2], "_") {
		return n, false
	}
	return n, true
}
