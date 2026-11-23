package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearer adds an access token to every request.
type bearer struct{ token *string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+*b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func postJSON(t *testing.T, u string, v any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(v)
	resp, err := http.Post(u, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func postForm(t *testing.T, u string, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, u string) map[string]any {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

var requestField = regexp.MustCompile(`name="request" value="([^"]*)"`)

// TestMCPOverOAuth runs what an MCP client does: discovery, registration,
// consent in the browser, the code exchange, tool calls, refresh and
// revocation.
func TestMCPOverOAuth(t *testing.T) {
	e := setupAuth(t)
	base := e.ts.URL
	put(t, e.st, acmeCall)
	put(t, e.st, "---\ntitle: Board prep\ndate: 2026-11-10T09:00:00-08:00\ntype: internal\nparticipants: [Antonio <ac@melzi.dev>]\n---\nWe discussed the runway and hiring.\n")

	// Without a token: 401 pointing at the resource metadata.
	resp, err := http.Post(base+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	prm := getJSON(t, base+"/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != base+"/mcp" || prm["authorization_servers"].([]any)[0] != base {
		t.Fatalf("resource metadata: %v", prm)
	}
	asm := getJSON(t, base+"/.well-known/oauth-authorization-server")
	if asm["authorization_endpoint"] != base+"/oauth/authorize" || asm["registration_endpoint"] != base+"/oauth/register" {
		t.Fatalf("server metadata: %v", asm)
	}

	// Registration.
	if code, _ := postJSON(t, base+"/oauth/register", map[string]any{"redirect_uris": []string{"javascript:alert(1)"}}); code != 400 {
		t.Fatalf("javascript: redirect accepted: %d", code)
	}
	redirect := "http://127.0.0.1:9999/callback"
	code, reg := postJSON(t, base+"/oauth/register", map[string]any{
		"client_name": "DeepSeek Harness", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "client_secret_basic",
	})
	if code != 201 || reg["token_endpoint_auth_method"] != "none" {
		t.Fatalf("register: %d %v", code, reg)
	}
	clientID := reg["client_id"].(string)

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	authorize := "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"resource": {base + "/mcp"}, "scope": {"mcp"},
	}.Encode()

	// A redirect_uri the app didn't register is refused on the page, not redirected to.
	b := e.browser()
	if code, body := e.signIn(t, b, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}},
		strings.Replace(authorize, url.QueryEscape(redirect), url.QueryEscape("http://evil.example/cb"), 1)); code != 400 || !strings.Contains(body, "didn&#39;t register") {
		t.Fatalf("unregistered redirect: %d", code)
	}

	// Consent page, signed in.
	code, body, resp := b.do(t, "GET", authorize, nil)
	if code != 200 || !strings.Contains(body, "Allow “DeepSeek Harness”?") || !strings.Contains(body, "127.0.0.1:9999") {
		t.Fatalf("consent page: %d %s", code, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://127.0.0.1:9999;") {
		t.Fatalf("CSP: %q", csp)
	}
	req := html.UnescapeString(requestField.FindStringSubmatch(body)[1])
	b.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Cancel sends access_denied back to the app.
	_, _, resp = b.do(t, "POST", "/oauth/authorize", url.Values{"request": {req}, "decision": {"deny"}})
	if loc, _ := url.Parse(resp.Header.Get("Location")); loc.Query().Get("error") != "access_denied" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("deny: %v", loc)
	}
	// Allow.
	_, _, resp = b.do(t, "POST", "/oauth/authorize", url.Values{"request": {req}, "decision": {"allow"}})
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "127.0.0.1:9999" || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != base || loc.Query().Get("code") == "" {
		t.Fatalf("allow: %v", loc)
	}
	authCode := loc.Query().Get("code")

	// The code needs the right verifier, and works once.
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "client_id": {clientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {base + "/mcp"}}
	wrong := url.Values{}
	for k, v := range exchange {
		wrong[k] = v
	}
	wrong.Set("code_verifier", strings.Repeat("w", 50))
	if code, out := postForm(t, base+"/oauth/token", wrong); code != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", code, out)
	}
	// The failed attempt used the code up: get another.
	_, _, resp = b.do(t, "POST", "/oauth/authorize", url.Values{"request": {req}, "decision": {"allow"}})
	loc, _ = url.Parse(resp.Header.Get("Location"))
	exchange.Set("code", loc.Query().Get("code"))
	code, toks := postForm(t, base+"/oauth/token", exchange)
	if code != 200 || toks["access_token"] == nil || toks["refresh_token"] == nil {
		t.Fatalf("token: %d %v", code, toks)
	}
	if code, _ := postForm(t, base+"/oauth/token", exchange); code != 400 {
		t.Fatal("a code worked twice")
	}

	// MCP with the token.
	access := toks["access_token"].(string)
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: bearer{&access}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if ins := session.InitializeResult().Instructions; !strings.Contains(ins, "acting for antonio") {
		t.Fatalf("instructions: %s", ins)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.Annotations.ReadOnlyHint != (tl.Name != "create_note") {
			t.Errorf("%s: read-only hint is %v", tl.Name, tl.Annotations.ReadOnlyHint)
		}
		if !strings.Contains(string(mustJSON(tl.InputSchema)), `"intent"`) {
			t.Errorf("%s has no intent argument: %s", tl.Name, mustJSON(tl.InputSchema))
		}
	}
	if strings.Join(names, ",") != "create_note,get_graph,get_note,get_transcript,list_calendars,list_events,list_note_labels,list_transcript_types,search_notes,search_transcripts" {
		t.Fatalf("tools: %v", names)
	}

	call := func(name string, args map[string]any) (map[string]any, string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			return nil, res.Content[0].(*mcp.TextContent).Text
		}
		var out map[string]any
		json.Unmarshal(mustJSON(res.StructuredContent), &out)
		return out, ""
	}

	if _, msg := call("search_transcripts", map[string]any{"intent": "  "}); !strings.Contains(msg, "intent is required") {
		t.Fatalf("blank intent: %q", msg)
	}
	out, msg := call("search_transcripts", map[string]any{"intent": "test", "query": "runway"})
	if msg != "" || out["total"] != 1.0 {
		t.Fatalf("search: %v %s", out, msg)
	}
	hit := out["transcripts"].([]any)[0].(map[string]any)
	if hit["title"] != "Board prep" || hit["type"] != "internal" || !strings.Contains(hit["snippet"].(string), "runway") {
		t.Fatalf("hit: %v", hit)
	}
	out, _ = call("search_transcripts", map[string]any{"intent": "test", "participant": "maxim@", "from": "2026-11-14", "to": "2026-11-14"})
	if out["total"] != 1.0 || out["transcripts"].([]any)[0].(map[string]any)["title"] != "Acme intro" {
		t.Fatalf("participant/date search: %v", out)
	}
	out, _ = call("search_transcripts", map[string]any{"intent": "test", "to": "2026-11-13"})
	if out["total"] != 1.0 {
		t.Fatalf("to: %v", out)
	}

	out, _ = call("get_transcript", map[string]any{"intent": "test", "id": hit["id"], "max_bytes": 10})
	if out["text"] != "We discuss" || out["next_offset"] != 10.0 {
		t.Fatalf("get page 1: %v", out)
	}
	out, _ = call("get_transcript", map[string]any{"intent": "test", "id": hit["id"], "offset": 10})
	if out["text"] != "ed the runway and hiring.\n" || out["next_offset"] != nil {
		t.Fatalf("get page 2: %v", out)
	}
	if _, msg := call("get_transcript", map[string]any{"intent": "test", "id": "000000000000"}); !strings.Contains(msg, "no transcript") {
		t.Fatalf("missing: %q", msg)
	}
	out, _ = call("list_transcript_types", map[string]any{"intent": "test"})
	if len(out["types"].([]any)) != 2 {
		t.Fatalf("types: %v", out)
	}

	// Calendars: Antonio's invite, linked to the Acme call.
	if code, _ := upload(t, b, "personal", invite); code != 200 {
		b.do(t, "GET", "/settings/calendar", nil) // creates the personal calendar
		if code, body := upload(t, b, "personal", invite); code != 200 {
			t.Fatalf("upload: %d %s", code, body)
		}
	}
	out, _ = call("list_calendars", map[string]any{"intent": "test"})
	cal := out["calendars"].([]any)[0].(map[string]any)
	if cal["key"] != "antonio/personal" || cal["yours"] != true {
		t.Fatalf("calendars: %v", out)
	}
	out, msg = call("list_events", map[string]any{"intent": "test", "from": "2026-11-14", "to": "2026-11-14", "query": "jane"})
	if msg != "" || out["total"] != 1.0 {
		t.Fatalf("events: %v %s", out, msg)
	}
	ev := out["events"].([]any)[0].(map[string]any)
	if ev["title"] != "Acme <> Melzi" || ev["guests"].([]any)[0].(map[string]any)["email"] != "jane@acme.com" {
		t.Fatalf("event: %v", ev)
	}
	if tr := ev["transcripts"].([]any); len(tr) != 1 || tr[0].(map[string]any)["title"] != "Acme intro" {
		t.Fatalf("linked transcripts: %v", ev["transcripts"])
	}
	if _, msg := call("list_events", map[string]any{"intent": "test", "calendars": []string{"nobody/x"}}); !strings.Contains(msg, "antonio/personal") {
		t.Fatalf("unknown calendar: %q", msg)
	}

	// Notes (no AI key here, so the title is the first line) and labels.
	if _, err := e.notes.PutLabel(ctx, "slop", "Pasted AI output, not reviewed", "antonio"); err != nil {
		t.Fatal(err)
	}
	nid, err := e.notes.Submit("Runway is 14 months\nHire in Q1.", "antonio", []string{"slop"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if n, _ := e.notes.Get(nid); !n.Pending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, msg = call("search_notes", map[string]any{"intent": "test", "query": "hire"})
	if msg != "" || out["total"] != 1.0 || out["notes"].([]any)[0].(map[string]any)["title"] != "Runway is 14 months" {
		t.Fatalf("search_notes: %v %s", out, msg)
	}
	out, _ = call("list_note_labels", map[string]any{"intent": "test"})
	if l := out["labels"].([]any); len(l) != 1 || l[0].(map[string]any)["meaning"] != "Pasted AI output, not reviewed" || l[0].(map[string]any)["notes"] != 1.0 {
		t.Fatalf("labels: %v", out)
	}
	if out, _ = call("search_notes", map[string]any{"intent": "test", "without_label": "slop"}); out["total"] != 0.0 {
		t.Fatalf("without_label: %v", out)
	}
	out, _ = call("get_note", map[string]any{"intent": "test", "id": nid})
	if l := out["labels"].([]any); len(l) != 1 || l[0] != "slop" {
		t.Fatalf("note labels: %v", out)
	}
	if out["text"] != "Runway is 14 months\nHire in Q1.\n" || out["author"] != "antonio" {
		t.Fatalf("get_note: %v", out)
	}

	// Writing a note: always slop, whatever the agent asks; attributed to the app.
	if _, err := e.notes.PutLabel(ctx, "competitors", "About other companies", "antonio"); err != nil {
		t.Fatal(err)
	}
	if _, msg := call("create_note", map[string]any{"intent": "test", "text": "x", "labels": []string{"nope"}}); !strings.Contains(msg, "no label") {
		t.Fatalf("unknown label: %q", msg)
	}
	if _, msg := call("create_note", map[string]any{"intent": "test", "text": "  "}); !strings.Contains(msg, "empty") {
		t.Fatalf("empty: %q", msg)
	}
	out, msg = call("create_note", map[string]any{"intent": "save the research", "text": "Acme charges per seat.", "title": "Acme pricing research", "labels": []string{"competitors"}})
	if msg != "" || out["pending"] != true || fmt.Sprint(out["labels"]) != "[slop competitors]" {
		t.Fatalf("create_note: %v %s", out, msg)
	}
	cid := out["id"].(string)
	for i := 0; i < 200; i++ {
		if n, _ := e.notes.Get(cid); !n.Pending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ = call("get_note", map[string]any{"intent": "test", "id": cid})
	if out["title"] != "Acme pricing research" || out["via"] != "MCP: DeepSeek Harness" || out["author"] != "antonio" || fmt.Sprint(out["labels"]) != "[slop competitors]" {
		t.Fatalf("created note: %v", out)
	}
	b.client.CheckRedirect = nil
	if _, body, _ := b.do(t, "GET", "/notes/"+cid, nil); !strings.Contains(body, "via MCP: DeepSeek Harness") || !strings.Contains(body, `class="label" href="/notes?label=slop"`) {
		t.Fatalf("note page: %s", body)
	}

	// Refresh: new tokens work, old ones don't.
	code, toks2 := postForm(t, base+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {toks["refresh_token"].(string)}, "client_id": {clientID}})
	if code != 200 {
		t.Fatalf("refresh: %d %v", code, toks2)
	}
	if code, _ := postForm(t, base+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {toks["refresh_token"].(string)}, "client_id": {clientID}}); code != 400 {
		t.Fatal("old refresh token still works")
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_transcript_types", Arguments: map[string]any{"intent": "x"}}); err == nil {
		t.Fatal("old access token still works")
	}
	access = toks2["access_token"].(string)
	// A failed call closes the client's session; a client reconnects.
	if session, err = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: bearer{&access}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, msg := call("list_transcript_types", map[string]any{"intent": "x"}); msg != "" {
		t.Fatal(msg)
	}

	// The app is listed in Settings → Tokens and can be revoked there.
	b.client.CheckRedirect = nil
	_, body, _ = b.do(t, "GET", "/settings/tokens", nil)
	m := regexp.MustCompile(`action="(/settings/apps/\d+/delete)"`).FindStringSubmatch(body)
	if !strings.Contains(body, "DeepSeek Harness") || m == nil {
		t.Fatalf("tokens page: %s", body)
	}
	if code, _, _ := b.do(t, "POST", m[1], url.Values{}); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_transcript_types", Arguments: map[string]any{"intent": "x"}}); err == nil {
		t.Fatal("revoked token still works")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
