//nolint:err113,goconst,gosec,tagliatelle,wrapcheck,wsl_v5
package googleapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	composioDefaultBaseURL = "https://backend.composio.dev"
	composioProxyPath      = "/api/v3.1/tools/execute/proxy"
	composioAccountsPath   = "/api/v3/connected_accounts"
)

var (
	composioHTTPClient = &http.Client{Timeout: 60 * time.Second}

	composioAccountCacheMu sync.Mutex
	composioAccountCache   = map[string]string{}
)

type composioProxyConfig struct {
	APIKey             string
	EntityID           string
	Email              string
	ServiceLabel       string
	ConnectedAccountID string
	BaseURL            string
}

type composioProxyTransport struct {
	baseTransport http.RoundTripper
	cfg           composioProxyConfig

	connectedAccountID string
	resolveOnce        sync.Once
	resolveErr         error
}

type composioParameter struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

type composioProxyRequest struct {
	Endpoint           string               `json:"endpoint"`
	Method             string               `json:"method"`
	ConnectedAccountID string               `json:"connected_account_id,omitempty"`
	Parameters         []composioParameter  `json:"parameters,omitempty"`
	Body               any                  `json:"body,omitempty"`
	BinaryBody         *composioBinaryInput `json:"binary_body,omitempty"`
}

type composioBinaryInput struct {
	Base64      string `json:"base64"`
	ContentType string `json:"content_type,omitempty"`
}

type composioProxyResponse struct {
	Data       json.RawMessage         `json:"data"`
	Status     int                     `json:"status"`
	Headers    map[string]any          `json:"headers"`
	BinaryData *composioBinaryResponse `json:"binary_data"`
	Error      any                     `json:"error"`
}

type composioBinaryResponse struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	ExpiresAt   string `json:"expires_at"`
}

type composioAccountsResponse struct {
	Items []composioAccount `json:"items"`
}

type composioAccount struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Toolkit    json.RawMessage `json:"toolkit"`
	AuthConfig json.RawMessage `json:"auth_config"`
	Raw        map[string]any  `json:"-"`
}

func (a *composioAccount) UnmarshalJSON(data []byte) error {
	type alias composioAccount
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err == nil {
		decoded.Raw = raw
	}
	*a = composioAccount(decoded)
	return nil
}

func composioProxyEnabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("GOG_COMPOSIO_PROXY")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func ComposioProxyEnabled() bool {
	return composioProxyEnabled()
}

func newComposioProxyConfig(serviceLabel string, email string) (composioProxyConfig, error) {
	apiKey := strings.TrimSpace(os.Getenv("COMPOSIO_API_KEY"))
	if apiKey == "" {
		return composioProxyConfig{}, errors.New("COMPOSIO_API_KEY is required when GOG_COMPOSIO_PROXY is enabled")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("GOG_COMPOSIO_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = composioDefaultBaseURL
	}

	entityID := firstNonEmpty(
		os.Getenv("GOG_COMPOSIO_ENTITY_ID"),
		os.Getenv("COMPOSIO_ENTITY_ID"),
	)

	return composioProxyConfig{
		APIKey:       apiKey,
		EntityID:     strings.TrimSpace(entityID),
		Email:        strings.TrimSpace(email),
		ServiceLabel: canonicalComposioServiceLabel(serviceLabel),
		ConnectedAccountID: strings.TrimSpace(firstNonEmpty(
			serviceEnv("GOG_COMPOSIO_%s_CONNECTED_ACCOUNT_ID", serviceLabel),
			serviceEnv("COMPOSIO_%s_CONNECTED_ACCOUNT_ID", serviceLabel),
			os.Getenv("GOG_COMPOSIO_CONNECTED_ACCOUNT_ID"),
			os.Getenv("COMPOSIO_CONNECTED_ACCOUNT_ID"),
		)),
		BaseURL: baseURL,
	}, nil
}

func newComposioProxyHTTPClient(serviceLabel string, email string) (*http.Client, error) {
	cfg, err := newComposioProxyConfig(serviceLabel, email)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: NewRetryTransport(newComposioProxyTransport(newBaseTransport(), cfg))}, nil
}

func NewComposioProxyHTTPClient(serviceLabel string, email string) (*http.Client, error) {
	return newComposioProxyHTTPClient(serviceLabel, email)
}

func newComposioProxyTransport(base http.RoundTripper, cfg composioProxyConfig) *composioProxyTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &composioProxyTransport{baseTransport: base, cfg: cfg}
}

func (t *composioProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isGoogleAPIRequest(req) {
		return t.baseTransport.RoundTrip(req)
	}

	accountID, err := t.connectedAccount(req.Context())
	if err != nil {
		return nil, err
	}

	proxyReq, err := t.buildProxyRequest(req, accountID)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(proxyReq)
	if err != nil {
		return nil, fmt.Errorf("marshal composio proxy request: %w", err)
	}

	proxyURL := t.cfg.BaseURL + composioProxyPath
	outReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, proxyURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create composio proxy request: %w", err)
	}
	outReq.Header.Set("Content-Type", "application/json")
	outReq.Header.Set("x-api-key", t.cfg.APIKey)
	outReq.Header.Set("User-Agent", "gogcli-composio-proxy/1.0")

	proxyResp, err := composioHTTPClient.Do(outReq)
	if err != nil {
		return nil, fmt.Errorf("composio proxy request: %w", err)
	}
	defer proxyResp.Body.Close()

	raw, err := io.ReadAll(proxyResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read composio proxy response: %w", err)
	}

	if proxyResp.StatusCode < 200 || proxyResp.StatusCode >= 300 {
		return httpResponse(req, proxyResp.StatusCode, proxyResp.Header, raw), nil
	}

	var decoded composioProxyResponse
	if unmarshalErr := json.Unmarshal(raw, &decoded); unmarshalErr != nil {
		return nil, fmt.Errorf("decode composio proxy response: %w", unmarshalErr)
	}

	status := decoded.Status
	if status == 0 {
		status = http.StatusOK
	}

	headers := headersFromComposio(decoded.Headers)
	body, contentType, err := t.responseBody(req.Context(), decoded)
	if err != nil {
		return nil, err
	}
	if contentType != "" && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", contentType)
	}

	return httpResponse(req, status, headers, body), nil
}

func (t *composioProxyTransport) connectedAccount(ctx context.Context) (string, error) {
	t.resolveOnce.Do(func() {
		t.connectedAccountID, t.resolveErr = t.resolveConnectedAccount(ctx)
	})
	return t.connectedAccountID, t.resolveErr
}

func (t *composioProxyTransport) resolveConnectedAccount(ctx context.Context) (string, error) {
	if t.cfg.ConnectedAccountID != "" {
		return t.cfg.ConnectedAccountID, nil
	}

	cacheKey := strings.Join([]string{t.cfg.BaseURL, t.cfg.APIKey, t.cfg.EntityID, t.cfg.Email, t.cfg.ServiceLabel}, "\x00")
	composioAccountCacheMu.Lock()
	if accountID := composioAccountCache[cacheKey]; accountID != "" {
		composioAccountCacheMu.Unlock()
		return accountID, nil
	}
	composioAccountCacheMu.Unlock()

	accounts, err := t.listConnectedAccounts(ctx)
	if err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", newComposioConnectorNotConnectedError(t.cfg.ServiceLabel)
	}

	serviceAccounts := t.filterAccountsForService(accounts)
	if len(serviceAccounts) == 0 {
		return "", newComposioConnectorNotConnectedError(t.cfg.ServiceLabel)
	}

	accountID := serviceAccounts[0].ID
	if desiredEmail := t.desiredEmail(); desiredEmail != "" && len(serviceAccounts) > 1 {
		if matched, ok := t.findAccountForEmail(ctx, serviceAccounts, desiredEmail); ok {
			accountID = matched
		} else {
			slog.Warn("no Composio Google account matched requested email; using first active service account", "service", t.cfg.ServiceLabel, "email", desiredEmail, "account", accountID)
		}
	}

	composioAccountCacheMu.Lock()
	composioAccountCache[cacheKey] = accountID
	composioAccountCacheMu.Unlock()
	return accountID, nil
}

func (t *composioProxyTransport) listConnectedAccounts(ctx context.Context) ([]composioAccount, error) {
	params := url.Values{"statuses": {"ACTIVE"}, "limit": {"100"}}
	if t.cfg.EntityID != "" && t.cfg.EntityID != "default" {
		params.Set("user_ids", t.cfg.EntityID)
	}
	accounts, err := t.fetchConnectedAccounts(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(accounts) > 0 || t.cfg.EntityID == "" || t.cfg.EntityID == "default" {
		return filterGoogleAccounts(accounts), nil
	}

	params.Del("user_ids")
	params.Set("user_id", t.cfg.EntityID)
	accounts, err = t.fetchConnectedAccounts(ctx, params)
	if err != nil {
		return nil, err
	}
	return filterGoogleAccounts(accounts), nil
}

func (t *composioProxyTransport) fetchConnectedAccounts(ctx context.Context, params url.Values) ([]composioAccount, error) {
	reqURL := t.cfg.BaseURL + composioAccountsPath + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create Composio accounts request: %w", err)
	}
	req.Header.Set("x-api-key", t.cfg.APIKey)
	req.Header.Set("User-Agent", "gogcli-composio-proxy/1.0")

	resp, err := composioHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list Composio connected accounts: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Composio accounts response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list Composio connected accounts: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var decoded composioAccountsResponse
	if err := json.Unmarshal(raw, &decoded); err == nil && decoded.Items != nil {
		return decoded.Items, nil
	}
	var items []composioAccount
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decode Composio accounts response: %w", err)
	}
	return items, nil
}

func (t *composioProxyTransport) findAccountForEmail(ctx context.Context, accounts []composioAccount, desiredEmail string) (string, bool) {
	for _, account := range accounts {
		email, err := t.gmailProfileEmail(ctx, account.ID)
		if err != nil {
			slog.Debug("Composio Gmail profile probe failed", "account", account.ID, "err", err)
			continue
		}
		if strings.EqualFold(email, desiredEmail) {
			return account.ID, true
		}
	}
	return "", false
}

func (t *composioProxyTransport) gmailProfileEmail(ctx context.Context, accountID string) (string, error) {
	body := composioProxyRequest{
		Endpoint:           "/gmail/v1/users/me/profile",
		Method:             http.MethodGet,
		ConnectedAccountID: accountID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.BaseURL+composioProxyPath, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", t.cfg.APIKey)

	resp, err := composioHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	var decoded struct {
		Status int `json:"status"`
		Data   struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	if decoded.Status < 200 || decoded.Status >= 300 {
		return "", fmt.Errorf("upstream status %d", decoded.Status)
	}
	if decoded.Data.EmailAddress == "" {
		return "", errors.New("empty Gmail profile email")
	}
	return decoded.Data.EmailAddress, nil
}

func (t *composioProxyTransport) buildProxyRequest(req *http.Request, accountID string) (composioProxyRequest, error) {
	endpoint := t.composioProxyEndpoint(req)

	parameters := make([]composioParameter, 0, len(req.URL.Query())+len(req.Header))
	for name, values := range req.URL.Query() {
		for _, value := range values {
			parameters = append(parameters, composioParameter{Name: name, Value: value, Type: "query"})
		}
	}
	for name, values := range req.Header {
		if shouldSkipProxyHeader(name) {
			continue
		}
		for _, value := range values {
			parameters = append(parameters, composioParameter{Name: name, Value: value, Type: "header"})
		}
	}

	proxyReq := composioProxyRequest{
		Endpoint:           endpoint,
		Method:             req.Method,
		ConnectedAccountID: accountID,
		Parameters:         parameters,
	}

	if req.Body == nil || req.Body == http.NoBody {
		return proxyReq, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return proxyReq, fmt.Errorf("read Google API request body: %w", err)
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))

	if len(body) == 0 {
		return proxyReq, nil
	}

	contentType := req.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" || strings.HasSuffix(mediaType, "/json") || strings.HasSuffix(mediaType, "+json") || mediaType == "application/json" {
		var decoded any
		if err := json.Unmarshal(body, &decoded); err == nil {
			proxyReq.Body = decoded
			return proxyReq, nil
		}
	}

	proxyReq.BinaryBody = &composioBinaryInput{
		Base64:      base64.StdEncoding.EncodeToString(body),
		ContentType: contentType,
	}
	return proxyReq, nil
}

func (t *composioProxyTransport) composioProxyEndpoint(req *http.Request) string {
	endpoint := req.URL.EscapedPath()
	if endpoint == "" {
		return "/"
	}
	if t.cfg.ServiceLabel == "calendar" {
		if strings.HasPrefix(endpoint, "/calendar/v3/") {
			return strings.TrimPrefix(endpoint, "/calendar/v3")
		}
		if endpoint == "/calendar/v3" {
			return "/"
		}
		return endpoint
	}
	if t.cfg.ServiceLabel == "docs" {
		if strings.HasPrefix(endpoint, "/v1/") {
			return strings.TrimPrefix(endpoint, "/v1")
		}
		if endpoint == "/v1" {
			return "/"
		}
		return endpoint
	}
	if t.cfg.ServiceLabel == "sheets" {
		if strings.HasPrefix(endpoint, "/v4/") {
			return strings.TrimPrefix(endpoint, "/v4")
		}
		if endpoint == "/v4" {
			return "/"
		}
		return endpoint
	}
	if t.cfg.ServiceLabel != "drive" {
		return endpoint
	}
	if isGoogleAPIRequest(req) && strings.HasPrefix(endpoint, "/upload/") {
		uploadURL := *req.URL
		uploadURL.RawQuery = ""
		uploadURL.Fragment = ""
		return uploadURL.String()
	}
	if strings.HasPrefix(endpoint, "/drive/v3/") {
		return strings.TrimPrefix(endpoint, "/drive/v3")
	}
	if endpoint == "/drive/v3" {
		return "/"
	}
	return endpoint
}

func (t *composioProxyTransport) responseBody(ctx context.Context, decoded composioProxyResponse) ([]byte, string, error) {
	if decoded.BinaryData != nil && decoded.BinaryData.URL != "" {
		body, contentType, err := fetchComposioBinary(ctx, decoded.BinaryData)
		if err != nil {
			return nil, "", err
		}
		return body, contentType, nil
	}

	if len(decoded.Data) == 0 || string(decoded.Data) == "null" {
		if decoded.Error != nil {
			body, err := json.Marshal(decoded.Error)
			if err != nil {
				return nil, "", err
			}
			return body, "application/json", nil
		}
		return []byte("{}"), "application/json", nil
	}
	return decoded.Data, "application/json", nil
}

func fetchComposioBinary(ctx context.Context, binary *composioBinaryResponse) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binary.URL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create Composio binary request: %w", err)
	}
	resp, err := composioHTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch Composio binary response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch Composio binary response: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read Composio binary response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = binary.ContentType
	}
	return body, contentType, nil
}

func httpResponse(req *http.Request, status int, header http.Header, body []byte) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func isGoogleAPIRequest(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	host := strings.ToLower(req.URL.Hostname())
	return host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com")
}

func shouldSkipProxyHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "host", "content-length", "user-agent":
		return true
	default:
		return false
	}
}

func filterGoogleAccounts(accounts []composioAccount) []composioAccount {
	filtered := make([]composioAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.Status != "" && !strings.EqualFold(account.Status, "ACTIVE") {
			continue
		}
		if isGoogleToolkitSlug(account.ToolkitSlug()) {
			filtered = append(filtered, account)
		}
	}
	return filtered
}

func (t *composioProxyTransport) filterAccountsForService(accounts []composioAccount) []composioAccount {
	service := canonicalComposioServiceLabel(t.cfg.ServiceLabel)
	authConfigIDs := serviceAuthConfigIDs(service)
	if len(authConfigIDs) > 0 {
		filtered := make([]composioAccount, 0, len(accounts))
		for _, account := range accounts {
			if authConfigIDs[account.AuthConfigID()] {
				filtered = append(filtered, account)
			}
		}
		return filtered
	}

	filtered := make([]composioAccount, 0, len(accounts))
	for _, account := range accounts {
		if account.ToolkitMatchesService(service) {
			filtered = append(filtered, account)
		}
	}
	return filtered
}

func (a composioAccount) ToolkitSlug() string {
	var toolkitString string
	if err := json.Unmarshal(a.Toolkit, &toolkitString); err == nil {
		return toolkitString
	}
	var toolkitObject struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(a.Toolkit, &toolkitObject); err == nil {
		if toolkitObject.Slug != "" {
			return toolkitObject.Slug
		}
		return toolkitObject.Name
	}
	return ""
}

func (a composioAccount) AuthConfigID() string {
	for _, key := range []string{"auth_config_id", "authConfigId", "auth_configId"} {
		if value, ok := rawString(a.Raw, key); ok {
			return value
		}
	}

	var authConfigString string
	if err := json.Unmarshal(a.AuthConfig, &authConfigString); err == nil {
		return strings.TrimSpace(authConfigString)
	}
	var authConfigObject map[string]any
	if err := json.Unmarshal(a.AuthConfig, &authConfigObject); err == nil {
		for _, key := range []string{"id", "auth_config_id", "authConfigId"} {
			if value, ok := rawString(authConfigObject, key); ok {
				return value
			}
		}
	}
	for _, key := range []string{"authConfig", "auth_config"} {
		if nested, ok := a.Raw[key].(map[string]any); ok {
			for _, nestedKey := range []string{"id", "auth_config_id", "authConfigId"} {
				if value, ok := rawString(nested, nestedKey); ok {
					return value
				}
			}
		}
	}
	return ""
}

func rawString(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func isGoogleToolkitSlug(slug string) bool {
	switch strings.ToLower(slug) {
	case "google", "gmail", "googlegmail", "googlesuper", "googlecalendar", "googledrive", "googlesheets", "googledocs":
		return true
	default:
		return false
	}
}

func (a composioAccount) ToolkitMatchesService(service string) bool {
	slug := strings.ToLower(a.ToolkitSlug())
	switch canonicalComposioServiceLabel(service) {
	case "gmail":
		return slug == "gmail" || slug == "googlegmail"
	case "calendar":
		return slug == "googlecalendar"
	case "drive":
		return slug == "googledrive"
	case "docs":
		return slug == "googledocs"
	case "sheets":
		return slug == "googlesheets"
	default:
		return slug == strings.ToLower(service)
	}
}

func canonicalComposioServiceLabel(service string) string {
	service = strings.ToLower(strings.TrimSpace(service))
	switch service {
	case "google-calendar", "cal":
		return "calendar"
	case "google-drive", "gdrive", "drv":
		return "drive"
	case "google-docs", "doc":
		return "docs"
	case "google-sheets", "sheet":
		return "sheets"
	case "mail", "email", "google-gmail":
		return "gmail"
	default:
		return service
	}
}

func serviceAuthConfigIDs(service string) map[string]bool {
	service = canonicalComposioServiceLabel(service)
	candidates := []string{
		serviceEnv("GOG_COMPOSIO_%s_AUTH_CONFIG_ID", service),
		serviceEnv("COMPOSIO_%s_AUTH_CONFIG_ID", service),
	}
	ids := map[string]bool{}
	for _, candidate := range candidates {
		for _, part := range strings.Split(candidate, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				ids[part] = true
			}
		}
	}
	return ids
}

func serviceEnv(format string, service string) string {
	service = canonicalComposioServiceLabel(service)
	if service == "" {
		return ""
	}
	return os.Getenv(fmt.Sprintf(format, envServiceName(service)))
}

func envServiceName(service string) string {
	service = strings.ToUpper(strings.TrimSpace(service))
	replacer := strings.NewReplacer("-", "_", ".", "_", " ", "_")
	return replacer.Replace(service)
}

type composioConnectorNotConnectedError struct {
	Service string
}

func (e *composioConnectorNotConnectedError) Error() string {
	label := composioServiceDisplayName(e.Service)
	if label == "" {
		label = "Google app"
	}
	return fmt.Sprintf("%s is not connected. Connect %s in Mally Settings > Connectors.", label, label)
}

func newComposioConnectorNotConnectedError(service string) error {
	return &composioConnectorNotConnectedError{Service: canonicalComposioServiceLabel(service)}
}

func composioServiceDisplayName(service string) string {
	switch canonicalComposioServiceLabel(service) {
	case "gmail":
		return "Gmail"
	case "calendar":
		return "Google Calendar"
	case "drive":
		return "Google Drive"
	case "docs":
		return "Google Docs"
	case "sheets":
		return "Google Sheets"
	default:
		return strings.TrimSpace(service)
	}
}

func (t *composioProxyTransport) desiredEmail() string {
	for _, value := range []string{t.cfg.Email, os.Getenv("GOG_ACCOUNT"), t.cfg.EntityID} {
		value = strings.TrimSpace(value)
		if value != "" && value != "me" && value != "default" && strings.Contains(value, "@") {
			return value
		}
	}
	return ""
}

func headersFromComposio(input map[string]any) http.Header {
	header := http.Header{}
	for name, value := range input {
		switch typed := value.(type) {
		case string:
			header.Add(name, typed)
		case []any:
			for _, item := range typed {
				if s, ok := item.(string); ok {
					header.Add(name, s)
				}
			}
		}
	}
	return header
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
