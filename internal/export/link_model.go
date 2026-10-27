package export

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
)

type triggerKind string

const (
	triggerNone       triggerKind = ""
	triggerHuman      triggerKind = "human"
	triggerBackground triggerKind = "background"
)

type thread struct {
	Turns []*turn
}

type turn struct {
	ID      string
	Trigger triggerKind
	Prompt  string
	Detail  string
	At, End time.Time
	Work    []*step
	Answer  string
	Asks    []qa
}

type qa struct{ Question, Answer string }

type stepKind string

const (
	stepNote   stepKind = "note"
	stepTool   stepKind = "tool"
	stepNotice stepKind = "notice"
)

type step struct {
	Kind stepKind
	Text string
	Body string
	Call *toolCall
}

type toolCall struct {
	id                string
	at                time.Time
	Name, Summary     string
	Args              string
	ArgsCut           bool
	Output            string
	OutSize           int
	OutCut, Done      bool
	URL               string
	Sub               *subagent
	agentID           string
	questions, answer string
}

type subagent struct {
	run    string
	Prompt string
	Report string
	Steps  []*step
}

type rowClass uint8

const (
	classDrop rowClass = iota
	classPrompt
	classBackground
	classNote
	classCall
	classResult
	classSkill
	classCompaction
	classContext
	classNotice
)

type event struct {
	class       rowClass
	text, title string
	id          string
}

type userPayload struct {
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	ID               string `json:"id"`
	Origin           struct {
		Kind string `json:"kind"`
	} `json:"origin"`
}

// userTextRules name the text a harness writes into the user's channel; text
// matching none is the human's.
var userTextRules = []struct {
	prefix string
	class  rowClass
}{
	{"<task-notification>", classBackground},
	{"<codex_internal_context", classBackground},
	{"Base directory for this skill: ", classSkill},
	{"This session is being continued from a previous conversation", classCompaction},
	{"(Re-invocation of /", classDrop},
	{"[Request interrupted by user", classNotice},
}

// payload is read only for user rows: isMeta and origin live there and
// nowhere in the row.
func classify(r *index.Row, payload func() string) event {
	switch {
	case r.ActorKind == "assistant" && r.EventKind == "message":
		return event{class: classNote, text: stripReminders(r.Text)}
	case r.ActorKind == "assistant" && r.EventKind == "tool_call":
		return event{class: classCall}
	case r.ActorKind == "tool":
		return event{class: classResult, text: stripReminders(r.Text)}
	case r.ActorKind == "user" && r.EventKind == "message":
		return classifyUser(stripReminders(r.Text), payload())
	case r.ActorKind == "system" && r.EventKind == "compacted_raw":
		return event{class: classCompaction, title: "Context compacted"}
	case r.ActorKind == "system" && r.PayloadType == "agent_message":
		return event{class: classNotice, title: "Message from a subagent", text: r.Text}
	}
	return event{class: classDrop}
}

func classifyUser(text, raw string) event {
	if text == "" {
		return event{class: classDrop}
	}
	var p userPayload
	_ = json.Unmarshal([]byte(raw), &p)
	ev := event{text: text, id: p.ID}
	ev.class = classPrompt
	for _, rule := range userTextRules {
		if strings.HasPrefix(text, rule.prefix) {
			ev.class = rule.class
			break
		}
	}
	if ev.class == classPrompt {
		if strings.HasPrefix(text, "<command-message>") {
			ev = commandPrompt(ev)
		} else if names, rest := stripHarness(text); len(names) > 0 {
			ev.text = rest
			if rest == "" {
				ev.class, ev.title = classContext, "Harness context: "+strings.Join(names, ", ")
			}
		}
	}
	switch {
	case p.IsCompactSummary:
		ev.class = classCompaction
	case p.IsMeta && ev.class != classSkill:
		ev.class = classDrop
	case p.Origin.Kind == "human" && ev.class != classPrompt:
		ev.class = classPrompt
	case p.Origin.Kind != "" && p.Origin.Kind != "human":
		ev.class = classBackground
	}
	switch ev.class {
	case classBackground:
		ev.title = backgroundTitle(text)
	case classSkill:
		first, _, _ := strings.Cut(strings.TrimPrefix(text, "Base directory for this skill: "), "\n")
		ev.title = "Loaded skill " + path.Base(strings.TrimSpace(first))
		ev.text = ""
	case classCompaction:
		ev.title = "Context compacted"
	case classNotice:
		ev.title, ev.text = strings.Trim(text, "[] \n"), ""
	case classContext:
		ev.text = ""
	}
	return ev
}

var tagOpen = regexp.MustCompile(`^<([A-Za-z_][\w-]*)[^>]*>`)

const agentsHeading = "# AGENTS.md instructions for "

// Codex sends its context as <tag>…</tag> elements and an AGENTS.md heading
// ahead of the human's words, in the same message.
func stripHarness(text string) ([]string, string) {
	var names []string
	for {
		if strings.HasPrefix(text, agentsHeading) {
			_, rest, _ := strings.Cut(text, "\n")
			names = append(names, "AGENTS.md")
			text = strings.TrimSpace(rest)
			continue
		}
		m := tagOpen.FindStringSubmatch(text)
		if m == nil {
			return names, text
		}
		end := strings.Index(text, "</"+m[1]+">")
		if end < 0 {
			return names, text
		}
		if !strings.EqualFold(m[1], "INSTRUCTIONS") || len(names) == 0 || names[len(names)-1] != "AGENTS.md" {
			names = append(names, m[1])
		}
		text = strings.TrimSpace(text[end+len(m[1])+3:])
	}
}

func tagText(text, tag string) string {
	_, rest, ok := strings.Cut(text, "<"+tag+">")
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, "</"+tag+">")
	return strings.TrimSpace(body)
}

func commandPrompt(ev event) event {
	if tagText(ev.text, "skill-format") == "true" {
		ev.class = classDrop
		return ev
	}
	name := tagText(ev.text, "command-name")
	if name == "" {
		name = "/" + tagText(ev.text, "command-message")
	}
	ev.class, ev.title = classPrompt, ""
	ev.text = strings.TrimSpace(name + " " + tagText(ev.text, "command-args"))
	return ev
}

func backgroundTitle(text string) string {
	if s := tagText(text, "summary"); s != "" {
		return s
	}
	if strings.HasPrefix(text, "<codex_internal_context") {
		return "Continue toward the goal"
	}
	if names, _ := stripHarness(text); len(names) > 0 {
		return names[0]
	}
	first, _, _ := strings.Cut(text, "\n")
	return clip(first, 120)
}

var reminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

func stripReminders(s string) string {
	if !strings.Contains(s, "<system-reminder>") {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(reminder.ReplaceAllString(s, ""))
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut, _ := cutBytes(s, n)
	return cut + "…"
}

var summaryArg = map[string]string{
	"Bash": "description", "Agent": "description", "Task": "description",
	"Read": "file_path", "Edit": "file_path", "Write": "file_path", "MultiEdit": "file_path",
	"NotebookEdit": "notebook_path", "Grep": "pattern", "Glob": "pattern",
	"WebFetch": "url", "WebSearch": "query", "Skill": "skill", "spawn_agent": "task_name",
}

func callSummary(name, input, text string) string {
	var args map[string]any
	if json.Unmarshal([]byte(input), &args) != nil || args == nil {
		return clip(firstLine(text), 120)
	}
	if s, ok := args[summaryArg[name]].(string); ok && s != "" {
		return clip(s, 120)
	}
	if s, ok := args["command"].(string); ok && s != "" {
		return clip(s, 120)
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" && len(s) <= 200 {
			return clip(s, 120)
		}
	}
	return ""
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

var agentIDText = regexp.MustCompile(`agentId: ([A-Za-z0-9_-]+)`)

// answeredQuestions reads AskUserQuestion's result, which quotes each
// question and the chosen answer as "question"="answer".
func answeredQuestions(input, result string) []qa {
	var args struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(input), &args) != nil {
		return nil
	}
	var out []qa
	for i, q := range args.Questions {
		key := fmt.Sprintf("%q=\"", q.Question)
		start := strings.Index(result, key)
		if start < 0 {
			continue
		}
		rest := result[start+len(key):]
		end := -1
		if i+1 < len(args.Questions) {
			end = strings.Index(rest, fmt.Sprintf("\", %q=", args.Questions[i+1].Question))
		}
		if end < 0 {
			end = strings.LastIndex(rest, "\".")
		}
		if end < 0 {
			continue
		}
		out = append(out, qa{Question: q.Question, Answer: rest[:end]})
	}
	return out
}

type runBuilder struct {
	th     *thread
	cur    *turn
	calls  map[string]*toolCall
	list   []*toolCall
	seen   map[string]bool
	first  time.Time
	parent string
}

type convBuilder struct {
	until   string
	jsonURL string
	runs    map[string]*runBuilder
	order   []string
	start   time.Time
}

func newConvBuilder(until, jsonURL string) *convBuilder {
	return &convBuilder{until: until, jsonURL: jsonURL, runs: map[string]*runBuilder{}}
}

func (b *convBuilder) run(id string) *runBuilder {
	rb := b.runs[id]
	if rb == nil {
		rb = &runBuilder{th: &thread{}, calls: map[string]*toolCall{}, seen: map[string]bool{}}
		b.runs[id] = rb
		b.order = append(b.order, id)
	}
	return rb
}

func (b *convBuilder) add(r *index.Row, payload func() string) {
	at := r.EventTime()
	stamp := isoMillis(at)
	if stamp == unstamped || stamp > b.until {
		return
	}
	if b.start.IsZero() {
		b.start = at
	}
	ev := classify(r, payload)
	if ev.class == classDrop {
		return
	}
	rb := b.run(r.AgentRunID)
	if rb.first.IsZero() {
		rb.first = at
	}
	if rb.parent == "" {
		rb.parent = r.ParentToolCallID
	}
	if ev.id != "" && (ev.class == classPrompt || ev.class == classBackground || ev.class == classContext) {
		if rb.seen[ev.id] {
			return
		}
		rb.seen[ev.id] = true
	}
	switch ev.class {
	case classPrompt, classBackground:
		t := &turn{Trigger: triggerHuman, Prompt: ev.text, At: at, End: at}
		if ev.class == classBackground {
			t.Trigger, t.Prompt, t.Detail = triggerBackground, ev.title, ev.text
		}
		rb.th.Turns = append(rb.th.Turns, t)
		rb.cur = t
		return
	}
	if ev.class == classResult {
		rb.result(r.ToolCallID, ev.text, at)
		return
	}
	t := rb.turn(at)
	t.End = at
	switch ev.class {
	case classNote:
		if ev.text != "" {
			t.Work = append(t.Work, &step{Kind: stepNote, Text: ev.text})
		}
	case classCall:
		c := &toolCall{id: r.ToolCallID, at: at, Name: r.ToolName, Summary: callSummary(r.ToolName, r.TioInput, r.Text), URL: b.jsonURL}
		args := r.TioInput
		if args == "" {
			args = r.Text
		}
		c.Args, c.ArgsCut = cutBytes(args, maxResultBytes)
		if r.ToolName == "AskUserQuestion" {
			c.questions = r.TioInput
		}
		rb.list = append(rb.list, c)
		if r.ToolCallID != "" {
			rb.calls[r.ToolCallID] = c
		}
		t.Work = append(t.Work, &step{Kind: stepTool, Call: c})
	case classSkill:
		if n := len(t.Work); n == 0 || t.Work[n-1].Kind != stepTool || t.Work[n-1].Call.Name != "Skill" {
			t.Work = append(t.Work, &step{Kind: stepNotice, Text: ev.title})
		}
	default:
		t.Work = append(t.Work, &step{Kind: stepNotice, Text: ev.title, Body: ev.text})
	}
}

func (rb *runBuilder) result(id, text string, at time.Time) {
	c := rb.calls[id]
	if c == nil {
		return
	}
	t := rb.turn(at)
	t.End = at
	c.Done, c.OutSize = true, len(text)
	c.Output, c.OutCut = cutBytes(text, maxResultBytes)
	if c.questions != "" {
		t.Asks = append(t.Asks, answeredQuestions(c.questions, text)...)
	}
	if c.Name == "Agent" || c.Name == "Task" {
		if m := agentIDText.FindStringSubmatch(text); m != nil {
			c.agentID = m[1]
		}
	}
}

func (rb *runBuilder) turn(at time.Time) *turn {
	if rb.cur == nil {
		rb.cur = &turn{At: at, End: at}
		rb.th.Turns = append(rb.th.Turns, rb.cur)
	}
	return rb.cur
}

func (t *turn) settle() {
	for i := len(t.Work) - 1; i >= 0; i-- {
		if t.Work[i].Kind == stepNote {
			t.Answer = t.Work[i].Text
			t.Work = append(t.Work[:i], t.Work[i+1:]...)
			return
		}
	}
}

func (b *convBuilder) finish(session int) *thread {
	for _, rb := range b.runs {
		for _, t := range rb.th.Turns {
			t.settle()
		}
	}
	var all []*toolCall
	for _, id := range b.order {
		all = append(all, b.runs[id].list...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	byCall := map[string]*toolCall{}
	byAgent := map[string]*toolCall{}
	for _, c := range all {
		if c.id != "" && byCall[c.id] == nil {
			byCall[c.id] = c
		}
		if c.agentID != "" && byAgent[c.agentID] == nil {
			byAgent[c.agentID] = c
		}
	}
	main := b.run("")
	for _, id := range b.order {
		if id == "" {
			continue
		}
		rb := b.runs[id]
		sub := subagentOf(id, rb.th)
		c := byCall[rb.parent]
		if c == nil {
			c = byAgent[id]
		}
		if c == nil || c.Sub != nil {
			c = &toolCall{Name: "Agent", Summary: "Subagent " + id, Done: true}
			t := main.turnAt(rb.first)
			t.Work = append(t.Work, &step{Kind: stepTool, Call: c})
		}
		c.Sub = sub
	}
	for i, t := range main.th.Turns {
		t.ID = fmt.Sprintf("s%dt%d", session, i+1)
	}
	return main.th
}

func (rb *runBuilder) turnAt(at time.Time) *turn {
	var found *turn
	for _, t := range rb.th.Turns {
		if !t.At.After(at) {
			found = t
		}
	}
	if found == nil {
		return rb.turn(at)
	}
	return found
}

func subagentOf(run string, th *thread) *subagent {
	s := &subagent{run: run}
	for i, t := range th.Turns {
		switch {
		case i == 0 && t.Trigger == triggerHuman:
			s.Prompt = t.Prompt
		case t.Trigger != triggerNone:
			body := t.Prompt
			if t.Detail != "" {
				body = t.Detail
			}
			s.Steps = append(s.Steps, &step{Kind: stepNotice, Text: "Message to the agent", Body: body})
		}
		s.Steps = append(s.Steps, t.Work...)
		if i == len(th.Turns)-1 {
			s.Report = t.Answer
		} else if t.Answer != "" {
			s.Steps = append(s.Steps, &step{Kind: stepNote, Text: t.Answer})
		}
	}
	return s
}

func (t *turn) toolCalls() int {
	n := 0
	for _, s := range t.Work {
		if s.Kind == stepTool {
			n++
		}
	}
	return n
}

func (t *turn) subagents() int {
	n := 0
	for _, s := range t.Work {
		if s.Kind == stepTool && s.Call.Sub != nil {
			n++
		}
	}
	return n
}

func countCalls(steps []*step) int {
	n := 0
	for _, s := range steps {
		if s.Kind == stepTool {
			n++
		}
	}
	return n
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func workedFor(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

func (t *turn) workStats() string {
	parts := []string{workedFor(t.End.Sub(t.At)), plural(t.toolCalls(), "tool call")}
	if n := t.subagents(); n > 0 {
		parts = append(parts, plural(n, "subagent"))
	}
	return strings.Join(parts, " · ")
}

func (t *turn) WorkSummary() string { return "Worked " + t.workStats() }

func (s *subagent) StepsSummary() string {
	return plural(countCalls(s.Steps), "tool call")
}

func (t *turn) Human() bool { return t.Trigger == triggerHuman }

func (t *turn) Background() bool { return t.Trigger == triggerBackground }

const promptClamp = 1200

func (t *turn) PromptHead() string {
	head, _ := splitPrompt(t.Prompt)
	return head
}

func (t *turn) PromptRest() string {
	_, rest := splitPrompt(t.Prompt)
	return rest
}

func splitPrompt(p string) (string, string) {
	if len(p) <= promptClamp+promptClamp/4 {
		return p, ""
	}
	head, _ := cutBytes(p, promptClamp)
	if i := strings.LastIndexAny(head, "\n "); i > promptClamp/2 {
		head = head[:i]
	}
	return head, p[len(head):]
}

func (t *turn) Title() string { return clip(firstLine(t.Prompt), 100) }

func buildConversation(es index.ExportSession, until, jsonURL string, session int) (*thread, time.Time, error) {
	b := newConvBuilder(until, jsonURL)
	err := es.EachRow(func(r *index.Row) error {
		var perr error
		b.add(r, func() string {
			var p string
			p, perr = es.Payload(r)
			return p
		})
		return perr
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	return b.finish(session), b.start, nil
}
