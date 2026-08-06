// Package micropub implements a Micropub (https://micropub.spec.indieweb.org/)
// endpoint that writes posts as Markdown files into the blogger posts directory.
package micropub

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"macbirdie.net/blogger/auth"
	"macbirdie.net/blogger/post"
)

// Server is a Micropub server that writes posts to disk as Markdown files.
type Server struct {
	DB       *sql.DB
	PostsDir string // directory where new post files are written
	SiteRoot string // e.g. "https://example.com/"
	DestExt  string // e.g. ".html" or ""
	OnChange func() // called (in a goroutine) after any write
}

// Handler returns an http.Handler serving the Micropub, token, and auth endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/micropub", s.handleMicropub)
	mux.HandleFunc("/micropub/token", s.handleToken)
	mux.HandleFunc("/micropub/auth", s.handleAuth)
	return mux
}

// ── Authorization endpoint ────────────────────────────────────────────────────

var authFormTmpl = template.Must(template.New("auth").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorize — {{.ClientID}}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:420px;margin:4rem auto;padding:0 1rem}
h1{font-size:1.25rem}
label{display:block;margin:.6rem 0 .1rem}
input[type=text],input[type=password]{width:100%;padding:.4rem;box-sizing:border-box}
.row{display:flex;gap:.5rem;margin-top:1rem}
.approve{background:#2563eb;color:#fff;padding:.5rem 1rem;border:none;cursor:pointer;border-radius:3px}
.deny{padding:.5rem 1rem;border:1px solid #ccc;cursor:pointer;border-radius:3px}
.meta{font-size:.85rem;color:#555;margin:.25rem 0}
</style>
</head>
<body>
<h1>Authorize access</h1>
<p class="meta"><strong>{{.ClientID}}</strong> is requesting access to post to your blog.</p>
<p class="meta">Scope: <code>{{.Scope}}</code></p>
{{if .Error}}<p style="color:red">{{.Error}}</p>{{end}}
<form method="POST">
  <input type="hidden" name="client_id"    value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="state"        value="{{.State}}">
  <input type="hidden" name="scope"        value="{{.Scope}}">
  <label>Username<input type="text"     name="username" autofocus required></label>
  <label>Password<input type="password" name="password"          required></label>
  <div class="row">
    <button class="approve" type="submit" name="approve" value="1">Approve</button>
    <button class="deny"    type="submit" name="approve" value="0">Deny</button>
  </div>
</form>
</body>
</html>`))

// handleAuth serves the IndieAuth authorization endpoint.
//
//   - GET  /micropub/auth — show the consent form
//   - POST /micropub/auth — validate credentials and redirect with an auth code
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.showAuthForm(w, r, "")
	case http.MethodPost:
		s.processAuth(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type authFormData struct {
	ClientID    string
	RedirectURI string
	State       string
	Scope       string
	Error       string
}

func authParams(r *http.Request) authFormData {
	q := r.URL.Query()
	scope := q.Get("scope")
	if scope == "" {
		scope = "create update delete"
	}
	return authFormData{
		ClientID:    q.Get("client_id"),
		RedirectURI: q.Get("redirect_uri"),
		State:       q.Get("state"),
		Scope:       scope,
	}
}

func (s *Server) showAuthForm(w http.ResponseWriter, r *http.Request, errMsg string) {
	data := authParams(r)
	data.Error = errMsg
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	authFormTmpl.Execute(w, data) //nolint:errcheck
}

func (s *Server) processAuth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	clientID := r.FormValue("client_id")
	redirectURI := r.FormValue("redirect_uri")
	state := r.FormValue("state")
	scope := r.FormValue("scope")
	if scope == "" {
		scope = "create update delete"
	}

	// User clicked Deny.
	if r.FormValue("approve") != "1" {
		dest := redirectURI + "?error=access_denied"
		if state != "" {
			dest += "&state=" + url.QueryEscape(state)
		}
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}

	// Validate credentials.
	username := r.FormValue("username")
	ok, err := auth.ValidateCredentials(s.DB, username, r.FormValue("password"))
	if err != nil {
		log.Printf("micropub: auth credentials: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Re-render form preserving GET params so the hidden fields are populated.
		r.URL.RawQuery = url.Values{
			"client_id":    {clientID},
			"redirect_uri": {redirectURI},
			"state":        {state},
			"scope":        {scope},
		}.Encode()
		s.showAuthForm(w, r, "Invalid username or password.")
		return
	}

	code, err := auth.CreateAuthCode(s.DB, username, clientID, redirectURI, scope)
	if err != nil {
		log.Printf("micropub: create auth code: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	dest := redirectURI + "?code=" + url.QueryEscape(code)
	if state != "" {
		dest += "&state=" + url.QueryEscape(state)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// ── Token endpoint ────────────────────────────────────────────────────────────

// handleToken serves the IndieAuth token endpoint.
//
//   - POST /micropub/token  grant_type=authorization_code — exchange auth code for Bearer token
//   - POST /micropub/token  action=revoke                 — revoke a token
//   - GET  /micropub/token  Authorization: Bearer …       — verify a token
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.verifyToken(w, r)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.FormValue("action") == "revoke" {
			s.revokeToken(w, r)
		} else {
			s.exchangeToken(w, r)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// exchangeToken exchanges an authorization code for a Bearer token.
func (s *Server) exchangeToken(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":             "unsupported_grant_type",
			"error_description": "only authorization_code grant type is supported",
		})
		return
	}

	username, scope, err := auth.ExchangeAuthCode(
		s.DB,
		r.FormValue("code"),
		r.FormValue("client_id"),
		r.FormValue("redirect_uri"),
	)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":             "invalid_grant",
			"error_description": err.Error(),
		})
		return
	}

	token, err := auth.CreateToken(s.DB, username, r.FormValue("client_id"), scope)
	if err != nil {
		log.Printf("micropub: create token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"access_token": token,
		"token_type":   "Bearer",
		"scope":        scope,
		"me":           strings.TrimRight(s.SiteRoot, "/") + "/",
	})
}

// revokeToken immediately invalidates a Bearer token.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if err := auth.RevokeToken(s.DB, token); err != nil {
		log.Printf("micropub: revoke token: %v", err)
	}
	// Always return 200 per OAuth2 revocation spec (RFC 7009).
	w.WriteHeader(http.StatusOK)
}

// verifyToken responds to a token introspection request (GET with Bearer token).
func (s *Server) verifyToken(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	info, err := auth.ValidateToken(s.DB, token)
	if err != nil {
		log.Printf("micropub: verify token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"me":        strings.TrimRight(s.SiteRoot, "/") + "/",
		"client_id": info.ClientID,
		"scope":     info.Scope,
	})
}

// ── Micropub endpoint ─────────────────────────────────────────────────────────

func (s *Server) handleMicropub(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleQuery(w, r)
	case http.MethodPost:
		if !s.authenticated(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Micropub"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		s.handlePost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) authenticated(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	// Only fall back to form body for non-JSON requests; reading FormValue on a
	// JSON request consumes r.Body before parseJSON gets to it.
	if token == "" && !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		r.ParseForm() //nolint:errcheck
		token = r.FormValue("access_token")
	}
	if token == "" {
		return false
	}
	info, err := auth.ValidateToken(s.DB, token)
	return err == nil && info != nil
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("q") {
	case "config":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"media-endpoint": nil,
			"syndicate-to":   []interface{}{},
		})
	default:
		writeJSON(w, http.StatusOK, map[string]interface{}{})
	}
}

// ── Request parsing ───────────────────────────────────────────────────────────

type mpRequest struct {
	Action     string
	URL        string
	HType      string // e.g. "entry", "page" (h- prefix stripped)
	Properties map[string][]interface{}
	Replace    map[string][]interface{}
	Add        map[string][]interface{}
	Delete     interface{}
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	var req mpRequest
	var err error

	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		err = parseJSON(r, &req)
	} else {
		err = parseForm(r, &req)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_request", "error_description": err.Error(),
		})
		return
	}

	switch req.Action {
	case "", "create":
		s.create(w, &req)
	case "update":
		s.update(w, &req)
	case "delete":
		s.softDelete(w, &req)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
	}
}

func parseJSON(r *http.Request, req *mpRequest) error {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return err
	}

	str := func(key string) string {
		raw, ok := body[key]
		if !ok {
			return ""
		}
		var s string
		json.Unmarshal(raw, &s) //nolint:errcheck
		return s
	}

	req.Action = str("action")
	req.URL = str("url")

	if raw, ok := body["type"]; ok {
		var types []string
		if json.Unmarshal(raw, &types) == nil && len(types) > 0 {
			req.HType = strings.TrimPrefix(types[0], "h-")
		}
	}
	if req.HType == "" {
		req.HType = "entry"
	}

	rawProps := func(key string) map[string][]interface{} {
		raw, ok := body[key]
		if !ok {
			return nil
		}
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		return normalizeProps(m)
	}

	req.Properties = rawProps("properties")
	req.Replace = rawProps("replace")
	req.Add = rawProps("add")

	if raw, ok := body["delete"]; ok {
		var v interface{}
		json.Unmarshal(raw, &v) //nolint:errcheck
		req.Delete = v
	}
	return nil
}

func parseForm(r *http.Request, req *mpRequest) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	req.Action = r.FormValue("action")
	req.URL = r.FormValue("url")
	req.HType = strings.TrimPrefix(r.FormValue("h"), "h-")
	if req.HType == "" {
		req.HType = "entry"
	}

	skip := map[string]bool{"h": true, "action": true, "url": true, "access_token": true}
	req.Properties = make(map[string][]interface{})
	for key, values := range r.Form {
		if skip[key] {
			continue
		}
		norm := strings.TrimSuffix(key, "[]")
		var ifaces []interface{}
		for _, v := range values {
			ifaces = append(ifaces, v)
		}
		req.Properties[norm] = ifaces
	}
	return nil
}

func normalizeProps(m map[string]interface{}) map[string][]interface{} {
	out := make(map[string][]interface{})
	for k, v := range m {
		switch val := v.(type) {
		case []interface{}:
			out[k] = val
		default:
			out[k] = []interface{}{v}
		}
	}
	return out
}

// ── Property helpers ──────────────────────────────────────────────────────────

// strProp returns the first value of a property as a string.
// Content objects ({html:…} or {markdown:…}) are unwrapped.
func strProp(props map[string][]interface{}, key string) string {
	vals, ok := props[key]
	if !ok || len(vals) == 0 {
		return ""
	}
	switch v := vals[0].(type) {
	case string:
		return v
	case map[string]interface{}:
		if md, ok := v["markdown"].(string); ok {
			return md
		}
		if html, ok := v["html"].(string); ok {
			return html
		}
	}
	return fmt.Sprintf("%v", vals[0])
}

func strsProp(props map[string][]interface{}, key string) []string {
	vals, ok := props[key]
	if !ok {
		return nil
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = fmt.Sprintf("%v", v)
	}
	return out
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// uniqueSlug returns a slug that does not collide with any existing file in dir.
// If <slug>.md is free it is returned unchanged; otherwise a timestamp suffix
// is appended: <slug>-20060102-150405, <slug>-20060102-150405-2, etc.
func uniqueSlug(dir, slug string) string {
	candidate := slug
	if !slugExists(dir, candidate) {
		return candidate
	}
	base := slug + "-" + time.Now().Format("20060102-150405")
	candidate = base
	for n := 2; slugExists(dir, candidate); n++ {
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
	return candidate
}

// slugExists returns true when any file named <slug>.<ext> exists in dir.
func slugExists(dir, slug string) bool {
	for _, ext := range []string{".md", ".markdown", ".txt"} {
		if _, err := os.Stat(filepath.Join(dir, slug+ext)); err == nil {
			return true
		}
	}
	return false
}

// ── Create ────────────────────────────────────────────────────────────────────

func (s *Server) create(w http.ResponseWriter, req *mpRequest) {
	props := req.Properties

	title := strProp(props, "name")
	content := strProp(props, "content")
	tags := strsProp(props, "category")
	isDraft := strProp(props, "post-status") == "draft"
	customSlug := strProp(props, "mp-slug")
	publishedStr := strProp(props, "published")

	// mp-type (e.g. mp-type=snippet) takes precedence; otherwise the type is
	// inferred from the h- value and whether a title is present (title-less
	// h-entry → Snippet, matching how the blog renders lightweight tweet-like posts).
	articleType := post.Post
	switch strings.ToLower(strProp(props, "mp-type")) {
	case "snippet":
		articleType = post.Snippet
	case "page":
		articleType = post.Page
	case "post":
		articleType = post.Post
	default:
		switch req.HType {
		case "snippet":
			articleType = post.Snippet
		case "page":
			articleType = post.Page
		case "entry":
			if title == "" {
				articleType = post.Snippet
			}
		}
	}

	var pubTime time.Time
	if publishedStr != "" {
		if t, err := time.Parse(time.RFC3339, publishedStr); err == nil {
			pubTime = t
		}
	}
	if pubTime.IsZero() {
		pubTime = time.Now()
	}

	// Build a desired slug then guarantee it is unique in the posts directory.
	desiredSlug := customSlug
	if desiredSlug == "" && title != "" {
		desiredSlug = slugify(title)
	}
	if desiredSlug == "" {
		desiredSlug = pubTime.Format("20060102-150405")
	}
	slug := uniqueSlug(s.PostsDir, desiredSlug)
	if slug != desiredSlug {
		log.Printf("micropub: slug %q already exists, using %q instead", desiredSlug, slug)
	}

	a := post.Article{
		Title:        title,
		Type:         articleType,
		DateModified: &pubTime,
		Draft:        isDraft,
		RawContent:   []byte(content),
	}
	for _, t := range tags {
		a.Tags = append(a.Tags, post.MakeTag(t))
	}

	destPath := filepath.Join(s.PostsDir, slug+".md")
	if err := os.WriteFile(destPath, []byte(formatArticle(a)), 0644); err != nil {
		log.Printf("micropub: write %s: %v", destPath, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("micropub: created %s", destPath)
	if s.OnChange != nil {
		go s.OnChange()
	}

	w.Header().Set("Location", s.postURL(slug, pubTime, isDraft))
	w.WriteHeader(http.StatusCreated)
}

// ── Update ────────────────────────────────────────────────────────────────────

func (s *Server) update(w http.ResponseWriter, req *mpRequest) {
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required for update"})
		return
	}

	srcPath, err := s.findSourceFile(req.URL)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "post not found"})
		return
	}

	a, err := readArticleFile(srcPath)
	if err != nil {
		log.Printf("micropub: read %s: %v", srcPath, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	applyReplace(a, req.Replace)
	applyAdd(a, req.Add)
	applyDelete(a, req.Delete)

	if err := os.WriteFile(srcPath, []byte(formatArticle(*a)), 0644); err != nil {
		log.Printf("micropub: write %s: %v", srcPath, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("micropub: updated %s", srcPath)
	if s.OnChange != nil {
		go s.OnChange()
	}
	w.WriteHeader(http.StatusNoContent)
}

func applyReplace(a *post.Article, props map[string][]interface{}) {
	for prop, vals := range props {
		setProp(a, prop, vals)
	}
}

func applyAdd(a *post.Article, props map[string][]interface{}) {
	for prop, vals := range props {
		if prop == "category" {
			for _, v := range vals {
				a.Tags = append(a.Tags, post.MakeTag(fmt.Sprintf("%v", v)))
			}
		}
	}
}

func applyDelete(a *post.Article, del interface{}) {
	switch d := del.(type) {
	case []interface{}:
		for _, prop := range d {
			clearProp(a, fmt.Sprintf("%v", prop))
		}
	case map[string]interface{}:
		for prop, rawVals := range d {
			if prop == "category" {
				if vSlice, ok := rawVals.([]interface{}); ok {
					removeTagValues(a, vSlice)
				}
			}
		}
	}
}

func setProp(a *post.Article, prop string, vals []interface{}) {
	if len(vals) == 0 {
		return
	}
	switch prop {
	case "name":
		a.Title = fmt.Sprintf("%v", vals[0])
	case "content":
		switch v := vals[0].(type) {
		case string:
			a.RawContent = []byte(v)
		case map[string]interface{}:
			if md, ok := v["markdown"].(string); ok {
				a.RawContent = []byte(md)
			} else if html, ok := v["html"].(string); ok {
				a.RawContent = []byte(html)
			}
		}
	case "category":
		a.Tags = nil
		for _, v := range vals {
			a.Tags = append(a.Tags, post.MakeTag(fmt.Sprintf("%v", v)))
		}
	case "published":
		if t, err := time.Parse(time.RFC3339, fmt.Sprintf("%v", vals[0])); err == nil {
			a.DateModified = &t
		}
	case "updated":
		if t, err := time.Parse(time.RFC3339, fmt.Sprintf("%v", vals[0])); err == nil {
			a.DateUpdated = &t
		}
	case "post-status":
		a.Draft = fmt.Sprintf("%v", vals[0]) == "draft"
	case "mp-type":
		switch strings.ToLower(fmt.Sprintf("%v", vals[0])) {
		case "snippet":
			a.Type = post.Snippet
		case "page":
			a.Type = post.Page
		case "post":
			a.Type = post.Post
		}
	case "summary":
		a.Description = fmt.Sprintf("%v", vals[0])
	}
}

func clearProp(a *post.Article, prop string) {
	switch prop {
	case "name":
		a.Title = ""
	case "category":
		a.Tags = nil
	case "summary":
		a.Description = ""
	case "updated":
		a.DateUpdated = nil
	}
}

func removeTagValues(a *post.Article, vals []interface{}) {
	remove := make(map[string]bool, len(vals))
	for _, v := range vals {
		remove[strings.ToLower(fmt.Sprintf("%v", v))] = true
	}
	kept := a.Tags[:0]
	for _, t := range a.Tags {
		if !remove[t.Name] {
			kept = append(kept, t)
		}
	}
	a.Tags = kept
}

// ── Delete (soft) ─────────────────────────────────────────────────────────────

// softDelete sets draft: true instead of removing the file so the post can be
// recovered. The static output is regenerated, removing it from the site.
func (s *Server) softDelete(w http.ResponseWriter, req *mpRequest) {
	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required for delete"})
		return
	}

	srcPath, err := s.findSourceFile(req.URL)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "post not found"})
		return
	}

	a, err := readArticleFile(srcPath)
	if err != nil {
		log.Printf("micropub: read %s: %v", srcPath, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	a.Draft = true
	if err := os.WriteFile(srcPath, []byte(formatArticle(*a)), 0644); err != nil {
		log.Printf("micropub: write %s: %v", srcPath, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("micropub: soft-deleted %s (draft: true)", srcPath)
	if s.OnChange != nil {
		go s.OnChange()
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── File helpers ──────────────────────────────────────────────────────────────

// findSourceFile resolves a published URL back to its source Markdown file by
// matching the slug (last path segment without extension) against filenames in
// the posts directory tree.
func (s *Server) findSourceFile(publishedURL string) (string, error) {
	u, err := url.Parse(publishedURL)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	base := filepath.Base(u.Path)
	slug := strings.TrimSuffix(base, filepath.Ext(base))

	var found string
	_ = filepath.Walk(s.PostsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		for _, ext := range []string{".md", ".markdown", ".txt"} {
			if strings.TrimSuffix(name, ext) == slug {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})

	if found == "" {
		return "", fmt.Errorf("source file not found for slug %q", slug)
	}
	return found, nil
}

func readArticleFile(path string) (*post.Article, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	a, err := post.ReadArticle(bufio.NewReader(f))
	return &a, err
}

func (s *Server) postURL(slug string, t time.Time, draft bool) string {
	root := strings.TrimRight(s.SiteRoot, "/")
	if draft {
		return root + "/drafts/" + slug + s.DestExt
	}
	return root + fmt.Sprintf("/%d/%02d/%s%s", t.Year(), t.Month(), slug, s.DestExt)
}

// formatArticle serialises an Article back to the frontmatter+body file format.
func formatArticle(a post.Article) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	if a.Title != "" {
		fmt.Fprintf(&sb, "title: %s\n", a.Title)
	}
	if a.Author != "" {
		fmt.Fprintf(&sb, "author: %s\n", a.Author)
	}
	switch a.Type {
	case post.Post:
		sb.WriteString("type: Post\n")
	case post.Page:
		sb.WriteString("type: Page\n")
	case post.Snippet:
		sb.WriteString("type: Snippet\n")
	}
	if a.DateModified != nil {
		fmt.Fprintf(&sb, "date: %s\n", a.DateModified.Format(time.RFC3339))
	}
	if a.DateUpdated != nil {
		fmt.Fprintf(&sb, "updated: %s\n", a.DateUpdated.Format(time.RFC3339))
	}
	if len(a.Tags) > 0 {
		names := make([]string, len(a.Tags))
		for i, t := range a.Tags {
			names[i] = t.OriginalName
		}
		fmt.Fprintf(&sb, "tags: %s\n", strings.Join(names, ", "))
	}
	if a.Description != "" {
		fmt.Fprintf(&sb, "description: %s\n", a.Description)
	}
	if a.Link != "" {
		fmt.Fprintf(&sb, "link: %s\n", a.Link)
	}
	if a.AppID != "" {
		fmt.Fprintf(&sb, "appid: %s\n", a.AppID)
	}
	if a.Draft {
		sb.WriteString("draft: true\n")
	}
	metaKeys := make([]string, 0, len(a.Meta))
	for k := range a.Meta {
		metaKeys = append(metaKeys, k)
	}
	sort.Strings(metaKeys)
	for _, k := range metaKeys {
		fmt.Fprintf(&sb, "meta-%s: %s\n", k, a.Meta[k])
	}
	sb.WriteString("---\n\n")
	sb.Write(a.RawContent)
	return sb.String()
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
