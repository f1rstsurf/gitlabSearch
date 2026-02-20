package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultGroupID = "XXXXXX"

type serverConfig struct {
	GitLabTokenDefault string
	GitLabGroupDefault string
	GitLabBaseURL      string
	GitLabWebURL       string
	ThemeDefault       string
	Port               string
}

type searchRequest struct {
	Pattern string `json:"pattern"`
	GroupID string `json:"group_id"`
	Token   string `json:"token"`
}

type blobSearchResult struct {
	ProjectID int    `json:"project_id"`
	Path      string `json:"path"`
	Ref       string `json:"ref"`
	StartLine int    `json:"startline"`
	Data      string `json:"data"`
}

type projectInfo struct {
	PathWithNamespace string `json:"path_with_namespace"`
}

type streamedResult struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Path   string `json:"path"`
	URL    string `json:"url"`
	Line   int    `json:"line"`
	Data   string `json:"data"`
}

func main() {
	cfg := loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search/stream", streamSearchHandler(cfg))
	mux.HandleFunc("/api/config", configHandler(cfg))
	mux.Handle("/", staticHandler())

	addr := ":" + cfg.Port
	log.Printf("gitlabSearch listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, withCORS(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() serverConfig {
	baseURL := strings.TrimSpace(os.Getenv("GITLAB_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://gitlab.com/api/v4"
	}

	webURL := strings.TrimSpace(os.Getenv("GITLAB_WEB_URL"))
	if webURL == "" {
		webURL = deriveWebURL(baseURL)
	}

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	groupDefault := strings.TrimSpace(os.Getenv("GITLAB_GROUP_ID"))
	if groupDefault == "" {
		groupDefault = defaultGroupID
	}

	return serverConfig{
		GitLabTokenDefault: strings.TrimSpace(os.Getenv("GITLAB_TOKEN")),
		GitLabGroupDefault: groupDefault,
		GitLabBaseURL:      baseURL,
		GitLabWebURL:       webURL,
		ThemeDefault:       normalizeTheme(os.Getenv("APP_THEME")),
		Port:               port,
	}
}

func normalizeTheme(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "dark":
		return "dark"
	default:
		return "light"
	}
}

func configHandler(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"theme_default": cfg.ThemeDefault,
		})
	}
}

func deriveWebURL(apiBase string) string {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(apiBase, "/"), "/api/v4")
	if trimmed == "" {
		return "https://gitlab.com"
	}
	return trimmed
}

func streamSearchHandler(cfg serverConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		var reqBody searchRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		pattern := strings.TrimSpace(reqBody.Pattern)
		if pattern == "" {
			http.Error(w, "pattern is required", http.StatusBadRequest)
			return
		}

		groupID := strings.TrimSpace(reqBody.GroupID)
		if groupID == "" {
			groupID = cfg.GitLabGroupDefault
		}
		token := strings.TrimSpace(reqBody.Token)
		if token == "" {
			token = cfg.GitLabTokenDefault
		}
		if token == "" {
			http.Error(w, "token is required (UI field or GITLAB_TOKEN env)", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ctx := r.Context()
		client := &http.Client{Timeout: 30 * time.Second}
		projectCache := map[int]string{}
		total := 0
		encoder := json.NewEncoder(w)

		if !writeMessage(encoder, flusher, map[string]any{
			"type":    "status",
			"message": fmt.Sprintf("Searching for %q in group %s", pattern, groupID),
		}) {
			return
		}

		for page := 1; ; page++ {
			if ctx.Err() != nil {
				return
			}

			results, nextPage, err := fetchBlobPage(ctx, client, cfg.GitLabBaseURL, token, groupID, pattern, page)
			if err != nil {
				writeMessage(encoder, flusher, map[string]any{"type": "error", "message": err.Error()})
				return
			}

			for _, item := range results {
				if ctx.Err() != nil {
					return
				}
				if item.ProjectID == 0 || item.Path == "" || item.Ref == "" {
					continue
				}

				repo, ok := projectCache[item.ProjectID]
				if !ok {
					repo, err = fetchProjectPath(ctx, client, cfg.GitLabBaseURL, token, item.ProjectID)
					if err != nil {
						writeMessage(encoder, flusher, map[string]any{"type": "error", "message": err.Error()})
						return
					}
					projectCache[item.ProjectID] = repo
				}

				line := item.StartLine
				if line <= 0 {
					line = 1
				}
				result := streamedResult{
					Repo:   repo,
					Branch: item.Ref,
					Path:   item.Path,
					URL:    fmt.Sprintf("%s/%s/-/blob/%s/%s#L%d", cfg.GitLabWebURL, repo, url.PathEscape(item.Ref), item.Path, line),
					Line:   line,
					Data:   item.Data,
				}

				total++
				if !writeMessage(encoder, flusher, map[string]any{"type": "result", "result": result}) {
					return
				}
			}

			if nextPage == "" {
				break
			}
			next, err := strconv.Atoi(nextPage)
			if err != nil || next <= page {
				break
			}
			page = next - 1
		}

		writeMessage(encoder, flusher, map[string]any{"type": "done", "total": total})
	}
}

func writeMessage(encoder *json.Encoder, flusher http.Flusher, payload map[string]any) bool {
	if err := encoder.Encode(payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func fetchBlobPage(ctx context.Context, client *http.Client, baseURL, token, groupID, pattern string, page int) ([]blobSearchResult, string, error) {
	u, err := url.Parse(baseURL + "/groups/" + groupID + "/search")
	if err != nil {
		return nil, "", fmt.Errorf("failed to build search url: %w", err)
	}
	q := u.Query()
	q.Set("scope", "blobs")
	q.Set("search", pattern)
	q.Set("per_page", "100")
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create search request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("search request failed on page %d: %w", page, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("gitlab search returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var results []blobSearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, "", fmt.Errorf("failed to parse search response: %w", err)
	}

	return results, strings.TrimSpace(resp.Header.Get("X-Next-Page")), nil
}

func fetchProjectPath(ctx context.Context, client *http.Client, baseURL, token string, projectID int) (string, error) {
	u := fmt.Sprintf("%s/projects/%d", baseURL, projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create project request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch project %d: %w", projectID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("gitlab project %d returned %d: %s", projectID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var info projectInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("failed to parse project %d response: %w", projectID, err)
	}
	if info.PathWithNamespace == "" {
		return "", fmt.Errorf("project %d has no namespace path", projectID)
	}
	return info.PathWithNamespace, nil
}

func staticHandler() http.Handler {
	root := filepath.Join("static")
	return http.FileServer(http.Dir(root))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
