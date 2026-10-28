// Package review runs a Sleipnir review of a pull request and writes what it found as GitHub comments: a walkthrough that
// stays up to date in one comment, and a review whose inline comments are the confirmed findings, each on the line where the
// change made it possible.
package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Report is what a Sleipnir review wrote: findings.json and map.json of .sleipnir/scan/SLUG.
type Report struct {
	Scan     string    `json:"scan"`
	Commit   string    `json:"commit"`
	Base     string    `json:"base"`
	Depth    string    `json:"depth"`
	Phase    string    `json:"phase"`
	Counts   Counts    `json:"counts"`
	Findings []Finding `json:"findings"`
	Changes  []struct {
		File  string   `json:"file"`
		Lines [][2]int `json:"lines"`
	} `json:"changes"`
	Map struct {
		Nodes []Node `json:"nodes"`
	} `json:"-"`
}

// Counts are the review's numbers.
type Counts struct {
	Files      int `json:"files"`
	Funcs      int `json:"funcs"`
	Lines      int `json:"lines"`
	Shards     int `json:"shards"`
	Mapped     int `json:"mapped"`
	Hunted     int `json:"hunted"`
	Points     int `json:"points"`
	Edges      int `json:"edges"`
	Leads      int `json:"leads"`
	Open       int `json:"open"`
	Confirmed  int `json:"confirmed"`
	Unproven   int `json:"unproven"`
	Rejected   int `json:"rejected"`
	Reproduced int `json:"reproduced"`
}

// Cite is a place in the code with the code there.
type Cite struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	Quote string `json:"quote"`
}

// Step is one step of a finding's trace.
type Step struct {
	Cite
	Note string `json:"note"`
}

// Finding is one lead as the review reports it.
type Finding struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Impact   string `json:"impact"`
	Reach    string `json:"reach"`
	Exploit  string `json:"exploit"`
	Claim    string `json:"claim"`
	Trace    []Step `json:"trace"`
	Fix      string `json:"fix"`
	By       string `json:"by"`
	Verdicts []struct {
		By      string `json:"by"`
		Role    string `json:"role"`
		Confirm bool   `json:"confirm"`
		Note    string `json:"note"`
	} `json:"verdicts"`
	Proofs []struct {
		By      string `json:"by"`
		Script  string `json:"script"`
		Outcome string `json:"outcome"`
		Output  string `json:"output"`
	} `json:"proofs"`
	Anchor *Cite `json:"anchor"`
}

// Node is a shard or a point of map.json.
type Node struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`
	Lane    string `json:"lane"`
	Summary string `json:"summary"`
}

// Load reads a review's report from its directory.
func Load(dir string) (*Report, error) {
	var r Report
	b, err := os.ReadFile(filepath.Join(dir, "findings.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("findings.json: %w", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "map.json")); err == nil {
		_ = json.Unmarshal(b, &r.Map)
	}
	return &r, nil
}

// Markers find the reviewer's own comments again: the walkthrough to edit it in place, and each finding so a later push does
// not post it twice.
const (
	WalkthroughMarker = "<!-- anemos-reviewer:walkthrough -->"
	findingMarker     = "<!-- anemos-reviewer:finding %s -->"
)

var findingRE = regexp.MustCompile(`<!-- anemos-reviewer:finding ([0-9a-f]{16}) -->`)

// Posted lists the fingerprints of findings already posted, from the bodies of the pull request's review comments.
func Posted(bodies []string) map[string]bool {
	out := map[string]bool{}
	for _, b := range bodies {
		for _, m := range findingRE.FindAllStringSubmatch(b, -1) {
			out[m[1]] = true
		}
	}
	return out
}

// Fingerprint identifies a finding across pushes: its category, file and the code at its anchor, not the line number, which
// moves when lines are added above it.
func Fingerprint(f Finding) string {
	c := f.at()
	h := sha256.Sum256([]byte(f.Category + "\x00" + c.File + "\x00" + strings.Join(strings.Fields(c.Quote), " ")))
	return hex.EncodeToString(h[:8])
}

// at is where a finding is shown: its anchor, else its sink.
func (f Finding) at() Cite {
	if f.Anchor != nil {
		return *f.Anchor
	}
	if len(f.Trace) > 0 {
		return f.Trace[len(f.Trace)-1].Cite
	}
	return Cite{}
}

var sevOrder = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

var sevLabel = map[string]string{"critical": "🔴 Critical", "high": "🟠 Major", "medium": "🟡 Minor", "low": "🔵 Trivial"}

// Inline is the review's inline comments' findings: the confirmed ones that have a place on the change, worst first.
func (r *Report) Inline() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Status == "confirmed" && f.Anchor != nil {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return sevOrder[out[i].Severity] < sevOrder[out[j].Severity] })
	return out
}

// Unproven are findings the verifiers confirmed that no reproducer showed yet: reported in the review's body, not inline.
func (r *Report) Unproven() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Status == "unproven" {
			out = append(out, f)
		}
	}
	return out
}

// Link points at a line of the reviewed commit.
func Link(repo, commit string, c Cite) string {
	end := c.Line + max(len(strings.Split(strings.TrimRight(c.Quote, "\n"), "\n"))-1, 0)
	frag := fmt.Sprintf("#L%d", c.Line)
	if end > c.Line {
		frag += fmt.Sprintf("-L%d", end)
	}
	return fmt.Sprintf("https://github.com/%s/blob/%s/%s%s", repo, commit, escapePath(c.File), frag)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = strings.NewReplacer("%", "%25", " ", "%20", "#", "%23", "?", "%3F").Replace(s)
	}
	return strings.Join(parts, "/")
}

// Comment is the inline comment of a finding: severity and category, the claim, the trace with links, how it was checked,
// the fix, and a prompt an agent can act on alone.
func Comment(repo, commit, bot string, f Finding) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("_%s_ · **%s** · reach %s, exploit %s\n\n", sevLabel[f.Severity], category(f.Category), f.Reach, f.Exploit)
	w("**%s**\n\n%s\n\n", oneLine(f.Title), strings.TrimSpace(f.Claim))
	if f.Impact != "" {
		w("Impact if exploited: %s.\n\n", f.Impact)
	}
	w("<details>\n<summary>Trace checked by the harness (%d steps)</summary>\n\n", len(f.Trace))
	for i, s := range f.Trace {
		w("%d. [`%s:%d`](%s) %s\n", i+1, s.File, s.Line, Link(repo, commit, s.Cite), oneLine(s.Note))
		w("%s", fence(s.Quote, "   "))
	}
	w("\n</details>\n\n")
	conf := 0
	defended := false
	for _, v := range f.Verdicts {
		if v.Confirm {
			conf++
			defended = defended || v.Role == "defender"
		}
	}
	w("Confirmed by %d %s other than the one that found it", conf, plural(conf, "agent", "agents"))
	if defended {
		w(", one of them arguing it was a false positive")
	}
	if p := reproduced(f); p != nil {
		w(", and reproduced in a sandbox.\n\n<details>\n<summary>Reproducer and its output</summary>\n\n%s\n%s</details>\n\n", fence(p.Script, ""), fence(p.Output, ""))
	} else {
		w(".\n\n")
	}
	if strings.TrimSpace(f.Fix) != "" {
		w("**Suggested fix:** %s\n\n", strings.TrimSpace(f.Fix))
	}
	w("<details>\n<summary>🤖 Prompt for AI agents</summary>\n\n%s</details>\n\n", fence(AgentPrompt(f), ""))
	w("<sub>Comment `@%s review` after a fix to check the pull request again.</sub>\n", bot)
	w(findingMarker+"\n", Fingerprint(f))
	return b.String()
}

// AgentPrompt is a self-contained task for a coding agent: what is wrong, where, and how to show the fix holds.
func AgentPrompt(f Finding) string {
	at := f.at()
	var b strings.Builder
	fmt.Fprintf(&b, "In %s around line %d: %s\n", at.File, at.Line, strings.TrimSpace(f.Claim))
	b.WriteString("Path from input to harm:\n")
	for _, s := range f.Trace {
		fmt.Fprintf(&b, "- %s:%d %s\n", s.File, s.Line, oneLine(s.Note))
	}
	if strings.TrimSpace(f.Fix) != "" {
		fmt.Fprintf(&b, "Fix: %s\n", strings.TrimSpace(f.Fix))
	}
	b.WriteString("Fix the cause on this path rather than the symptom, add a test that fails without the fix, and run the project's tests.\n")
	return b.String()
}

func reproduced(f Finding) *struct {
	By      string `json:"by"`
	Script  string `json:"script"`
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
} {
	for i := range f.Proofs {
		if f.Proofs[i].Outcome == "reproduced" && f.Proofs[i].By != f.By {
			return &f.Proofs[i]
		}
	}
	return nil
}

// Walkthrough is the summary comment: what the change does part by part (the review's shard maps), what was reviewed, and what
// came of it. head is the commit it describes; state is a line such as "Review in progress" shown above the rest.
func Walkthrough(repo string, r *Report, state string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("%s\n## Walkthrough\n\n", WalkthroughMarker)
	if state != "" {
		w("> %s\n\n", state)
	}
	if r == nil {
		return b.String()
	}
	var shards []Node
	for _, n := range r.Map.Nodes {
		if n.Kind == "shard" && strings.TrimSpace(n.Summary) != "" {
			shards = append(shards, n)
		}
	}
	if len(shards) > 0 {
		w("| Part | Summary |\n|---|---|\n")
		for _, n := range shards {
			w("| %s | %s |\n", cell(n.Label), cell(n.Summary))
		}
		w("\n")
	}
	c := r.Counts
	w("Reviewed %d %s (%d functions, %d lines) changed from `%s`, in %d parts: %d entry points, guards and sinks mapped, %d %s filed.\n\n",
		c.Files, plural(c.Files, "file", "files"), c.Funcs, c.Lines, short(r.Base), c.Shards, c.Points, c.Leads, plural(c.Leads, "lead", "leads"))
	inline := r.Inline()
	switch {
	case len(inline) > 0:
		w("**%d actionable %s**, each confirmed by other agents", len(inline), plural(len(inline), "finding", "findings"))
		if c.Reproduced > 0 {
			w(" and reproduced in a sandbox where the depth asks for it")
		}
		w(": see the review comments.\n\n")
	case r.Phase == "done":
		w("No actionable findings: nothing the change brings in held up under verification.\n\n")
	}
	if un := r.Unproven(); len(un) > 0 {
		w("<details>\n<summary>%d more confirmed by verifiers, not reproduced</summary>\n\n", len(un))
		for _, f := range un {
			at := f.at()
			w("- %s **%s** at [`%s:%d`](%s): %s\n", sevLabel[f.Severity], oneLine(f.Title), at.File, at.Line, Link(repo, r.Commit, at), oneLine(f.Claim))
		}
		w("\n</details>\n\n")
	}
	if c.Rejected > 0 {
		w("%d %s rejected by verifiers and not posted.\n\n", c.Rejected, plural(c.Rejected, "lead was", "leads were"))
	}
	if r.Phase != "done" {
		w("⚠️ The review stopped before it finished (phase `%s`): %d %s still open. Only confirmed findings are posted.\n\n", r.Phase, c.Open, plural(c.Open, "lead is", "leads are"))
	}
	w("<details>\n<summary>Review info</summary>\n\n- Commit: `%s`\n- Base: `%s`\n- Depth: %s\n- Run: `%s`\n\n</details>\n", short(r.Commit), short(r.Base), r.Depth, r.Scan)
	return b.String()
}

// ReviewBody is the body of the review that carries the inline comments.
func ReviewBody(repo string, r *Report, posted int) string {
	return fmt.Sprintf("**Actionable comments posted: %d**\n\nEach was confirmed by agents other than the one that found it; see the walkthrough for the rest of the review of `%s`.\n", posted, short(r.Commit))
}

func category(c string) string {
	c = strings.ReplaceAll(c, "-", " ")
	if c == "" {
		return c
	}
	return strings.ToUpper(c[:1]) + c[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cell(s string) string {
	s = oneLine(s)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

// fence wraps text in a code fence longer than any run of backticks in it, each line indented by indent.
func fence(s, indent string) string {
	s = strings.TrimRight(s, "\n")
	ticks := "```"
	for strings.Contains(s, ticks) {
		ticks += "`"
	}
	var b strings.Builder
	b.WriteString(indent + ticks + "\n")
	for _, l := range strings.Split(s, "\n") {
		b.WriteString(indent + l + "\n")
	}
	b.WriteString(indent + ticks + "\n")
	return b.String()
}
