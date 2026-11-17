package oauth

import "testing"

func TestCheckRedirectURI(t *testing.T) {
	for uri, ok := range map[string]bool{
		"https://app.example/cb":           true,
		"http://127.0.0.1:33418/callback":  true,
		"http://100.64.0.25:3080/oauth/cb": true,
		"http://[::1]:8080/cb":             true,
		"cursor://anysphere.cursor-mcp/cb": true,
		"com.example.app:/oauth":           true,
		"javascript:alert(1)":              false,
		"data:text/html,hi":                false,
		"https://app.example/cb#frag":      false,
		"/relative":                        false,
		"https://a.example;script-src/cb":  false,
		"https://a.example cb":             false,
	} {
		if err := CheckRedirectURI(uri); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok = %v", uri, err, ok)
		}
	}
	if o := RedirectOrigin("http://127.0.0.1:9/x?y"); o != "http://127.0.0.1:9" {
		t.Errorf("origin = %q", o)
	}
	if o := RedirectOrigin("com.example.app:/oauth"); o != "com.example.app:" {
		t.Errorf("origin = %q", o)
	}
}
