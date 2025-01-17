package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"io/ioutil"
	"net/http"

	"github.com/ysugimoto/doorkeeper/entity"
	"github.com/ysugimoto/doorkeeper/github"
	"github.com/ysugimoto/doorkeeper/rule"
)

const (
	githubEventNamePullRequest = "pull_request"
	githubEventNamePing        = "ping"
	githubEventNamePush        = "push"

	githubPullRequestActionOpened      = "opened"
	githubPullRequestActionEdited      = "edited"
	githubPullRequestActionSynchronize = "synchronize"
)

func WebhookHandler(prefix string, c *github.Client) http.Handler {
	if c == nil {
		c = github.DefaultClient
	}

	return http.StripPrefix(
		fmt.Sprintf("/%s", strings.Trim(prefix, "/")),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}

			// Check webhook request comes from exact Github server
			if !compareSignature(r) {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, "Signature unmatched")
				return
			}

			// Switch action by header
			switch r.Header.Get("X-Github-Event") {
			case githubEventNamePullRequest:
				// Accept PullRequest event
				var evt entity.GithubPullRequestEvent
				if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, "Failed to decode github webhook body to JSON "+err.Error())
					return
				}

				// Get and parse rule from destination repository
				rr, err := c.RuleFile(r.Context(), evt.ContentURL(github.SettingFile))
				if err != nil {
					rr = rule.DefaultRule
				}

				// switch actions by action
				switch evt.Action {

				// When new pullrequest has been opened, run validate and factory relates note
				case githubPullRequestActionOpened:
					if !rr.Validation.Disable {
						go validatePullRequest(c, evt, rr)
					}
					if ok, _ := rr.MatchValidateBranch(evt.BaseBranch()); ok {
						if !rr.ReleaseNote.Disable {
							go factoryRelaseNotes(c, evt, rr)
						}
					}

				// When pullrequest has been edited, only runs validate
				case githubPullRequestActionEdited:
					if !rr.Validation.Disable {
						go validatePullRequest(c, evt, rr)
					}

				// When pullrequest has been synchronized, only runs factory release notes
				case githubPullRequestActionSynchronize:
					if ok, _ := rr.MatchReleaseNoteBranch(evt.BaseBranch()); ok {
						if !rr.ReleaseNote.Disable {
							go factoryRelaseNotes(c, evt, rr)
						}
					}
				}
				successResponse(w)
				return

			case githubEventNamePush:
				// Accept push event
				var evt entity.GithubPushEvent
				if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, "Failed to decode github webhook body to JSON "+err.Error())
					return
				}

				// Get and parse rule from destination repository
				rr, err := c.RuleFile(r.Context(), evt.ContentURL("/.doorkeeper.yml"))
				if err != nil {
					rr = rule.DefaultRule
				}

				switch {
				case strings.HasPrefix(evt.Ref, "refs/tags"):
					if ok, _ := rr.MatchTag(strings.TrimPrefix(evt.Ref, "refs/tags/")); ok {
						if !rr.ReleaseNote.Disable {
							go processTagPushEvent(c, evt, rr)
						}
					}
				}
				successResponse(w)
				return

			case githubEventNamePing:
				// Accept Ping event
				successResponse(w)
				return
			}

			// Forbid other events
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "We don't support event of '"+r.Header.Get("X-Github-Event")+"'")
		}),
	)
}

func successResponse(w http.ResponseWriter) {
	message := "Accepted"

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", fmt.Sprint(len(message)))
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, message)
}

// compares webhook request signature with secret
func compareSignature(r *http.Request) bool {
	buf := new(bytes.Buffer)
	io.Copy(buf, r.Body)

	// Rewind request body
	defer func() {
		r.Body = ioutil.NopCloser(buf)
	}()

	mac := hmac.New(sha256.New, []byte(os.Getenv("WEBHOOK_SECRET")))
	mac.Write(buf.Bytes())
	expected := append([]byte("sha256="), []byte(fmt.Sprintf("%x", mac.Sum(nil)))...)
	return hmac.Equal(expected, []byte(r.Header.Get("X-Hub-Signature-256")))
}

// Integration for slack
func sendToSlack(ctx context.Context, webhookURL, message string) error {
	body, err := json.Marshal(map[string]string{
		"text": message,
	})
	if err != nil {
		return fmt.Errorf("Failed to marshal body: %w", err)
	}

	c, timeout := context.WithTimeout(ctx, 5*time.Second)
	defer timeout()

	req, err := http.NewRequestWithContext(c, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("Failed to make slack request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to get slack response: %w", err)
	}
	resp.Body.Close()
	return nil
}
