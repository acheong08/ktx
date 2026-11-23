package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/acheong08/ktx/internal/notes"
)

// waitNote waits for a note written in the UI to be saved.
func (e *authEnv) waitNote(t *testing.T, title string) notes.Note {
	t.Helper()
	for i := 0; i < 300; i++ {
		all, _ := e.notes.List()
		for _, n := range all {
			if n.Title == title && !n.Pending {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("note %q not saved", title)
	return notes.Note{}
}

func TestDecisionGraphUI(t *testing.T) {
	e := setupAuth(t)
	ant, max := e.browser(), e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	e.signIn(t, max, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")
	call := put(t, e.st, "---\ntitle: AI Software Factories\ndate: 2026-11-10T10:00:00-08:00\nparticipants: [Antonio <ac@melzi.dev>]\n---\nAdvisors get you into the right rooms.\n")

	// The scratchpad has the goal/decision block.
	_, body, _ := ant.do(t, "GET", "/notes", nil)
	if !strings.Contains(body, "data-graph-fields") || !strings.Contains(body, `name="kind"`) {
		t.Fatalf("no graph block:\n%s", body)
	}
	// A private goal; a plain note; a decision based on both and on the call.
	ant.do(t, "POST", "/notes", url.Values{"body": {"Make money, retire early."}, "title": {"Retire by 30"}, "kind": {"goal"},
		"status": {"active", "considering"}, "strength": {"hard"}, "private": {"1"}})
	retire := e.waitNote(t, "Retire by 30")
	if retire.Kind != "goal" || retire.Status != "active" || !retire.Private || retire.Strength != "hard" {
		t.Fatalf("goal: %+v", retire.Fields)
	}
	ant.do(t, "POST", "/notes", url.Values{"body": {"Security, infra, quant."}, "title": {"Our backgrounds"}})
	bg := e.waitNote(t, "Our backgrounds")
	code, body, _ := ant.do(t, "POST", "/notes", url.Values{"body": {"Cyber it is."}, "title": {"Work in cybersecurity"}, "kind": {"decision"},
		"status": {"active", "made"}, "strength": {"firm"},
		"ref": {"note:" + bg.ID, "note:" + retire.ID, "transcript:" + call}, "quote": {"", "", "Advisors get you into the right rooms."}})
	if code != 200 {
		t.Fatalf("decision: %d %s", code, body)
	}
	cyber := e.waitNote(t, "Work in cybersecurity")
	if cyber.Status != "made" || len(cyber.BasedOn) != 3 || cyber.BasedOn[2].Quote != "Advisors get you into the right rooms." {
		t.Fatalf("decision: %+v", cyber.Fields)
	}
	// A bad link is refused, keeping the form.
	if code, body, _ := ant.do(t, "POST", "/notes", url.Values{"body": {"x"}, "ref": {"note:000000000000"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "no note 000000000000") {
		t.Fatalf("bad link: %d", code)
	}

	// The link picker finds notes and calls.
	if _, body, _ = ant.do(t, "GET", "/graph/refs?q=factories", nil); !strings.Contains(body, `data-add-ref="transcript:`+call+`"`) {
		t.Fatalf("picker:\n%s", body)
	}
	if _, body, _ = max.do(t, "GET", "/graph/refs?q=retire", nil); strings.Contains(body, retire.ID) {
		t.Fatal("picker shows antonio's private goal to maxim")
	}

	// A task for the decision.
	ant.do(t, "POST", "/tasks", url.Values{"title": {"Find advisors"}})
	_, body, _ = ant.do(t, "GET", "/tasks", nil)
	link := regexp.MustCompile(`<a href="(/tasks/antonio/personal/[^"]+)">Find advisors</a>`).FindStringSubmatch(body)[1]
	_, body, _ = ant.do(t, "GET", link, nil)
	if !strings.Contains(body, `<option value="note:`+cyber.ID+`">Decision: Work in cybersecurity</option>`) {
		t.Fatalf("for picker:\n%s", body)
	}
	etag := regexp.MustCompile(`name="etag" value="([^"]+)"`).FindStringSubmatch(body)[1]
	ant.do(t, "POST", link, url.Values{"etag": {etag}, "title": {"Find advisors"}, "for": {"note:" + cyber.ID}})
	if _, body, _ = ant.do(t, "GET", "/tasks", nil); !strings.Contains(body, "→ Work in cybersecurity") {
		t.Fatalf("task list:\n%s", body)
	}

	// The graph, as each person sees it.
	_, body, _ = ant.do(t, "GET", "/graph", nil)
	for _, want := range []string{">Retire by 30<", ">Work in cybersecurity<", "Call: AI Software Factories", "Advisors get you into the right rooms.", ">Find advisors<", "Note: Our backgrounds"} {
		if !strings.Contains(body, want) {
			t.Fatalf("antonio's graph lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, ">Retire by 30<") > strings.Index(body, ">Work in cybersecurity<") {
		t.Fatal("decision isn't under the goal")
	}
	_, body, _ = max.do(t, "GET", "/graph", nil)
	if strings.Contains(body, "Retire by 30") || !strings.Contains(body, ">Work in cybersecurity<") {
		t.Fatalf("maxim's graph:\n%s", body)
	}
	// Private notes are hidden from others everywhere.
	if code, _, _ := max.do(t, "GET", "/notes/"+retire.ID, nil); code != http.StatusNotFound {
		t.Fatalf("maxim opened a private note: %d", code)
	}
	if _, body, _ = max.do(t, "GET", "/notes", nil); strings.Contains(body, "Retire by 30") {
		t.Fatal("private note in maxim's list")
	}
	if code, _, _ := max.do(t, "POST", "/notes/"+retire.ID+"/delete", url.Values{"confirm": {"on"}}); code != http.StatusNotFound {
		t.Fatalf("maxim deleted a private note: %d", code)
	}
	// The decision's page: based on (the private goal shows as unavailable to maxim), tasks.
	_, body, _ = max.do(t, "GET", "/notes/"+cyber.ID, nil)
	if !strings.Contains(body, "note:"+retire.ID+" (not available)") || !strings.Contains(body, ">Find advisors<") || !strings.Contains(body, "Advisors get you into the right rooms.") {
		t.Fatalf("decision page:\n%s", body)
	}
	// The call's page and the background note's page show what's based on them.
	if _, body, _ = ant.do(t, "GET", "/transcripts/"+call, nil); !strings.Contains(body, "Based on this call") || !strings.Contains(body, "Decision: Work in cybersecurity") {
		t.Fatalf("transcript page:\n%s", body)
	}
	if _, body, _ = ant.do(t, "GET", "/notes/"+bg.ID, nil); !strings.Contains(body, "Based on this") || !strings.Contains(body, "/notes?based_on=note:"+bg.ID) {
		t.Fatalf("note page:\n%s", body)
	}
	// Prefilled scratchpad from "add based on this".
	if _, body, _ = ant.do(t, "GET", "/notes?based_on=transcript:"+call, nil); !strings.Contains(body, `name="ref" value="transcript:`+call+`"`) || !strings.Contains(body, "Call: AI Software Factories") {
		t.Fatalf("prefill:\n%s", body)
	}
	// Kind filter.
	if _, body, _ = ant.do(t, "GET", "/notes?kind=goal", nil); !strings.Contains(body, "Retire by 30") || strings.Contains(body, "Our backgrounds") {
		t.Fatalf("kind filter:\n%s", body)
	}
	// Edit: a loop is refused.
	code, body, _ = ant.do(t, "POST", "/notes/"+retire.ID, url.Values{"title": {"Retire by 30"}, "body": {"x"}, "kind": {"goal"}, "ref": {"note:" + cyber.ID}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "loop") {
		t.Fatalf("loop: %d", code)
	}

	// MCP, as maxim: the graph without the private goal.
	m := mcpServerForTest(t, e)
	out := m.call(t, "maxim", "get_graph", map[string]any{"intent": "test"})
	outline := out["outline"].(string)
	if strings.Contains(outline, "Retire") || !strings.Contains(outline, "[decision · made · firm] Work in cybersecurity (note:"+cyber.ID+")") ||
		!strings.Contains(outline, `"Advisors get you into the right rooms."`) || !strings.Contains(outline, "tasks: [ ] Find advisors") {
		t.Fatalf("maxim's mcp graph:\n%s", outline)
	}
	if out := m.call(t, "antonio", "get_graph", map[string]any{"intent": "test"}); !strings.Contains(out["outline"].(string), "[goal · active · hard · private] Retire by 30") {
		t.Fatalf("antonio's mcp graph:\n%s", out["outline"])
	}
	if out := m.call(t, "maxim", "search_notes", map[string]any{"intent": "test", "kind": "goal"}); out["total"] != 0.0 {
		t.Fatalf("maxim sees private goals: %v", out)
	}
	if out := m.call(t, "maxim", "get_note", map[string]any{"intent": "test", "id": retire.ID}); out["error"] == nil {
		t.Fatalf("maxim read a private note: %v", out)
	}
}

// mcpTester calls MCP tools over /mcp with an OAuth grant per user.
type mcpTester struct {
	e      *authEnv
	client string
}

func mcpServerForTest(t *testing.T, e *authEnv) mcpTester {
	c, err := e.db.CreateOAuthClient("Graph test", []string{"http://127.0.0.1:9/cb"})
	if err != nil {
		t.Fatal(err)
	}
	return mcpTester{e: e, client: c.ID}
}

func (m mcpTester) call(t *testing.T, user, tool string, args map[string]any) map[string]any {
	t.Helper()
	u, err := m.e.db.UserByUsername(user)
	if err != nil {
		t.Fatal(err)
	}
	toks, err := m.e.db.CreateOAuthGrant(u.ID, m.client, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: m.e.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{&toks.Access}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		return map[string]any{"error": res.Content[0].(*mcp.TextContent).Text}
	}
	var out map[string]any
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	return out
}
