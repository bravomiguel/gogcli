//nolint:wsl_v5
package googleapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComposioProxyTransport_RoundTripProxiesGoogleRequest(t *testing.T) {
	origClient := composioHTTPClient
	origCache := composioAccountCache
	t.Cleanup(func() {
		composioHTTPClient = origClient
		composioAccountCache = origCache
	})
	composioAccountCache = map[string]string{}

	var sawProxy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case composioAccountsPath:
			_, _ = w.Write([]byte(`{"items":[{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`))
		case composioProxyPath:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode proxy request: %v", err)
			}
			if body["endpoint"] == "/gmail/v1/users/me/messages" {
				sawProxy = true
				if body["method"] != http.MethodPost {
					t.Fatalf("method = %v", body["method"])
				}
				if body["connected_account_id"] != "ca_1" {
					t.Fatalf("connected account = %v", body["connected_account_id"])
				}
				params := body["parameters"].([]any)
				if !hasParam(params, "q", "in:inbox", "query") {
					t.Fatalf("missing query param: %#v", params)
				}
				_, _ = w.Write([]byte(`{"status":200,"headers":{"content-type":"application/json"},"data":{"id":"msg_1"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":200,"data":{"emailAddress":"miguel@example.com"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	composioHTTPClient = srv.Client()

	transport := newComposioProxyTransport(nil, composioProxyConfig{
		APIKey:       "key",
		EntityID:     "miguel@example.com",
		Email:        "miguel@example.com",
		ServiceLabel: "gmail",
		BaseURL:      srv.URL,
	})
	req := httptest.NewRequest(http.MethodPost, "https://gmail.googleapis.com/gmail/v1/users/me/messages?q=in%3Ainbox", strings.NewReader(`{"raw":"abc"}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if !sawProxy {
		t.Fatalf("proxy endpoint was not called")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(raw)) != `{"id":"msg_1"}` {
		t.Fatalf("body = %s", raw)
	}
}

func TestComposioProxyTransport_PreservesGoogleUploadEndpoint(t *testing.T) {
	transport := newComposioProxyTransport(nil, composioProxyConfig{ServiceLabel: "drive"})
	req := httptest.NewRequest(
		http.MethodPost,
		"https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&supportsAllDrives=true",
		strings.NewReader("multipart body"),
	)
	req.Header.Set("Content-Type", "multipart/related; boundary=abc123")

	proxyReq, err := transport.buildProxyRequest(req, "ca_drive")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}

	if proxyReq.Endpoint != "https://www.googleapis.com/upload/drive/v3/files" {
		t.Fatalf("endpoint = %q", proxyReq.Endpoint)
	}
	if proxyReq.Method != http.MethodPost {
		t.Fatalf("method = %q", proxyReq.Method)
	}
	if proxyReq.ConnectedAccountID != "ca_drive" {
		t.Fatalf("connected account = %q", proxyReq.ConnectedAccountID)
	}
	if proxyReq.BinaryBody == nil {
		t.Fatalf("expected binary body")
	}
	if proxyReq.BinaryBody.ContentType != "multipart/related; boundary=abc123" {
		t.Fatalf("content type = %q", proxyReq.BinaryBody.ContentType)
	}
	raw, err := base64.StdEncoding.DecodeString(proxyReq.BinaryBody.Base64)
	if err != nil {
		t.Fatalf("decode binary body: %v", err)
	}
	if string(raw) != "multipart body" {
		t.Fatalf("binary body = %q", raw)
	}
	params := paramsToAny(proxyReq.Parameters)
	if !hasParam(params, "uploadType", "multipart", "query") {
		t.Fatalf("missing uploadType query param: %#v", proxyReq.Parameters)
	}
	if !hasParam(params, "supportsAllDrives", "true", "query") {
		t.Fatalf("missing supportsAllDrives query param: %#v", proxyReq.Parameters)
	}
	if !hasParam(params, "Content-Type", "multipart/related; boundary=abc123", "header") {
		t.Fatalf("missing content type header: %#v", proxyReq.Parameters)
	}
}

func TestComposioProxyTransport_TrimsDriveBasePath(t *testing.T) {
	transport := newComposioProxyTransport(nil, composioProxyConfig{ServiceLabel: "drive"})
	req := httptest.NewRequest(http.MethodGet, "https://www.googleapis.com/drive/v3/files?pageSize=1", nil)

	proxyReq, err := transport.buildProxyRequest(req, "ca_drive")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}

	if proxyReq.Endpoint != "/files" {
		t.Fatalf("endpoint = %q", proxyReq.Endpoint)
	}
	params := paramsToAny(proxyReq.Parameters)
	if !hasParam(params, "pageSize", "1", "query") {
		t.Fatalf("missing pageSize query param: %#v", proxyReq.Parameters)
	}
}

func TestComposioProxyTransport_TrimsCalendarBasePath(t *testing.T) {
	transport := newComposioProxyTransport(nil, composioProxyConfig{ServiceLabel: "calendar"})
	req := httptest.NewRequest(http.MethodGet, "https://www.googleapis.com/calendar/v3/users/me/calendarList?maxResults=1", nil)

	proxyReq, err := transport.buildProxyRequest(req, "ca_calendar")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}

	if proxyReq.Endpoint != "/users/me/calendarList" {
		t.Fatalf("endpoint = %q", proxyReq.Endpoint)
	}
	params := paramsToAny(proxyReq.Parameters)
	if !hasParam(params, "maxResults", "1", "query") {
		t.Fatalf("missing maxResults query param: %#v", proxyReq.Parameters)
	}
}

func TestComposioProxyTransport_TrimsDocsBasePath(t *testing.T) {
	transport := newComposioProxyTransport(nil, composioProxyConfig{ServiceLabel: "docs"})
	req := httptest.NewRequest(http.MethodGet, "https://docs.googleapis.com/v1/documents/doc_1?includeTabsContent=true", nil)

	proxyReq, err := transport.buildProxyRequest(req, "ca_docs")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}

	if proxyReq.Endpoint != "/documents/doc_1" {
		t.Fatalf("endpoint = %q", proxyReq.Endpoint)
	}
	params := paramsToAny(proxyReq.Parameters)
	if !hasParam(params, "includeTabsContent", "true", "query") {
		t.Fatalf("missing includeTabsContent query param: %#v", proxyReq.Parameters)
	}
}

func TestComposioProxyTransport_TrimsSheetsBasePath(t *testing.T) {
	transport := newComposioProxyTransport(nil, composioProxyConfig{ServiceLabel: "sheets"})
	req := httptest.NewRequest(http.MethodGet, "https://sheets.googleapis.com/v4/spreadsheets/sheet_1/values/Sheet1!A1%3AB2?majorDimension=ROWS", nil)

	proxyReq, err := transport.buildProxyRequest(req, "ca_sheets")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}

	if proxyReq.Endpoint != "/spreadsheets/sheet_1/values/Sheet1!A1%3AB2" {
		t.Fatalf("endpoint = %q", proxyReq.Endpoint)
	}
	params := paramsToAny(proxyReq.Parameters)
	if !hasParam(params, "majorDimension", "ROWS", "query") {
		t.Fatalf("missing majorDimension query param: %#v", proxyReq.Parameters)
	}
}

func TestComposioProxyTransport_SelectsMatchingGmailProfile(t *testing.T) {
	origClient := composioHTTPClient
	origCache := composioAccountCache
	t.Cleanup(func() {
		composioHTTPClient = origClient
		composioAccountCache = origCache
	})
	composioAccountCache = map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case composioAccountsPath:
			_, _ = w.Write([]byte(`{"items":[{"id":"ca_wrong","status":"ACTIVE","toolkit":{"slug":"gmail"}},{"id":"ca_right","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`))
		case composioProxyPath:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode proxy request: %v", err)
			}
			account := body["connected_account_id"]
			email := "other@example.com"
			if account == "ca_right" {
				email = "miguel@example.com"
			}
			_, _ = w.Write([]byte(`{"status":200,"data":{"emailAddress":"` + email + `"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	composioHTTPClient = srv.Client()

	transport := newComposioProxyTransport(nil, composioProxyConfig{
		APIKey:       "key",
		EntityID:     "miguel@example.com",
		Email:        "miguel@example.com",
		ServiceLabel: "gmail",
		BaseURL:      srv.URL,
	})
	accountID, err := transport.connectedAccount(t.Context())
	if err != nil {
		t.Fatalf("connectedAccount: %v", err)
	}
	if accountID != "ca_right" {
		t.Fatalf("accountID = %q", accountID)
	}
}

func TestComposioProxyTransport_ErrorsWhenMultipleGmailAccountsAreAmbiguous(t *testing.T) {
	origClient := composioHTTPClient
	origCache := composioAccountCache
	t.Cleanup(func() {
		composioHTTPClient = origClient
		composioAccountCache = origCache
	})
	composioAccountCache = map[string]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case composioAccountsPath:
			_, _ = w.Write([]byte(`{"items":[{"id":"ca_one","status":"ACTIVE","toolkit":{"slug":"gmail"}},{"id":"ca_two","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`))
		case composioProxyPath:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode proxy request: %v", err)
			}
			email := "one@example.com"
			if body["connected_account_id"] == "ca_two" {
				email = "two@example.com"
			}
			_, _ = w.Write([]byte(`{"status":200,"data":{"emailAddress":"` + email + `"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	composioHTTPClient = srv.Client()

	transport := newComposioProxyTransport(nil, composioProxyConfig{
		APIKey:       "key",
		EntityID:     "user_123",
		Email:        "user_123",
		ServiceLabel: "gmail",
		BaseURL:      srv.URL,
	})
	_, err := transport.connectedAccount(t.Context())
	if err == nil {
		t.Fatalf("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "multiple Gmail Composio accounts are connected") {
		t.Fatalf("error = %q", err.Error())
	}
	if !strings.Contains(err.Error(), "one@example.com\tca_one") || !strings.Contains(err.Error(), "two@example.com\tca_two") {
		t.Fatalf("error did not include account choices: %q", err.Error())
	}
}

func TestComposioProxyTransport_SelectsContextConnectedAccountID(t *testing.T) {
	transport := newComposioProxyTransport(nil, composioProxyConfig{
		APIKey:             "key",
		EntityID:           "user_123",
		Email:              "user_123",
		ServiceLabel:       "gmail",
		ConnectedAccountID: "ca_context",
		BaseURL:            "https://example.invalid",
	})
	accountID, err := transport.connectedAccount(t.Context())
	if err != nil {
		t.Fatalf("connectedAccount: %v", err)
	}
	if accountID != "ca_context" {
		t.Fatalf("accountID = %q", accountID)
	}
}

func TestListComposioProxyAccountsIncludesGmailProfiles(t *testing.T) {
	origClient := composioHTTPClient
	origCache := composioAccountCache
	t.Cleanup(func() {
		composioHTTPClient = origClient
		composioAccountCache = origCache
	})
	composioAccountCache = map[string]string{}
	t.Setenv("COMPOSIO_API_KEY", "key")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case composioAccountsPath:
			_, _ = w.Write([]byte(`{"items":[{"id":"ca_one","status":"ACTIVE","toolkit":{"slug":"gmail"}},{"id":"ca_two","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`))
		case composioProxyPath:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode proxy request: %v", err)
			}
			email := "one@example.com"
			if body["connected_account_id"] == "ca_two" {
				email = "two@example.com"
			}
			_, _ = w.Write([]byte(`{"status":200,"data":{"emailAddress":"` + email + `"}}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	composioHTTPClient = srv.Client()
	t.Setenv("GOG_COMPOSIO_BASE_URL", srv.URL)

	accounts, err := ListComposioProxyAccounts(t.Context(), "gmail", "two@example.com")
	if err != nil {
		t.Fatalf("ListComposioProxyAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d", len(accounts))
	}
	if accounts[0].EmailAddress != "one@example.com" || accounts[1].EmailAddress != "two@example.com" {
		t.Fatalf("accounts = %#v", accounts)
	}
	if !accounts[1].Selected || accounts[1].SelectionMatch != "email" {
		t.Fatalf("expected second account selected by email: %#v", accounts[1])
	}
}

func TestOptionsForAccountScopes_ComposioProxyBypassesLocalSecrets(t *testing.T) {
	t.Setenv("GOG_COMPOSIO_PROXY", "1")
	t.Setenv("COMPOSIO_API_KEY", "key")
	t.Setenv("GOG_COMPOSIO_CONNECTED_ACCOUNT_ID", "ca_1")

	opts, err := optionsForAccountScopes(t.Context(), "gmail", "miguel@example.com", []string{"scope"})
	if err != nil {
		t.Fatalf("optionsForAccountScopes: %v", err)
	}
	if len(opts) == 0 {
		t.Fatalf("expected client option")
	}
}

func TestComposioProxyTransport_SelectsAccountByServiceAuthConfig(t *testing.T) {
	origClient := composioHTTPClient
	origCache := composioAccountCache
	t.Cleanup(func() {
		composioHTTPClient = origClient
		composioAccountCache = origCache
	})
	composioAccountCache = map[string]string{}
	t.Setenv("GOG_COMPOSIO_CALENDAR_AUTH_CONFIG_ID", "ac_calendar")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case composioAccountsPath:
			_, _ = w.Write([]byte(`{"items":[{"id":"ca_gmail","status":"ACTIVE","toolkit":{"slug":"googlesuper"},"auth_config":{"id":"ac_gmail"}},{"id":"ca_calendar","status":"ACTIVE","toolkit":{"slug":"googlesuper"},"auth_config":{"id":"ac_calendar"}}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	composioHTTPClient = srv.Client()

	transport := newComposioProxyTransport(nil, composioProxyConfig{
		APIKey:       "key",
		EntityID:     "miguel@example.com",
		Email:        "miguel@example.com",
		ServiceLabel: "calendar",
		BaseURL:      srv.URL,
	})
	accountID, err := transport.connectedAccount(t.Context())
	if err != nil {
		t.Fatalf("connectedAccount: %v", err)
	}
	if accountID != "ca_calendar" {
		t.Fatalf("accountID = %q", accountID)
	}
}

func TestComposioProxyTransport_MissingServiceAccountErrorsClearly(t *testing.T) {
	origClient := composioHTTPClient
	origCache := composioAccountCache
	t.Cleanup(func() {
		composioHTTPClient = origClient
		composioAccountCache = origCache
	})
	composioAccountCache = map[string]string{}
	t.Setenv("GOG_COMPOSIO_GMAIL_AUTH_CONFIG_ID", "ac_gmail")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case composioAccountsPath:
			_, _ = w.Write([]byte(`{"items":[{"id":"ca_calendar","status":"ACTIVE","toolkit":{"slug":"googlesuper"},"auth_config":{"id":"ac_calendar"}}]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	composioHTTPClient = srv.Client()

	transport := newComposioProxyTransport(nil, composioProxyConfig{
		APIKey:       "key",
		EntityID:     "miguel@example.com",
		Email:        "miguel@example.com",
		ServiceLabel: "gmail",
		BaseURL:      srv.URL,
	})
	_, err := transport.connectedAccount(t.Context())
	if err == nil {
		t.Fatalf("expected error")
	}
	if err.Error() != "Gmail is not connected. Connect Gmail in Mally Settings > Connectors." {
		t.Fatalf("error = %q", err.Error())
	}
}

func hasParam(params []any, name string, value string, typ string) bool {
	for _, param := range params {
		m, ok := param.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == name && m["value"] == value && m["type"] == typ {
			return true
		}
	}
	return false
}

func paramsToAny(params []composioParameter) []any {
	out := make([]any, 0, len(params))
	for _, param := range params {
		out = append(out, map[string]any{
			"name":  param.Name,
			"value": param.Value,
			"type":  param.Type,
		})
	}
	return out
}
