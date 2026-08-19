package gh

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ── PR metadata ───────────────────────────────────────────────────────────────

type PR struct {
	Number  int
	Title   string
	Body    string
	Owner   string
	Repo    string
	HeadSHA string
}

// ── threads + messages ────────────────────────────────────────────────────────

// Message is a single entry in a NoteThread.
// FromGH=true means it came from GitHub (read-only, not published again).
type Message struct {
	Author    string
	Body      string
	CreatedAt time.Time
	FromGH    bool
}

// NoteThread is a conversation anchored to a file + line.
// Line=0 means a file-level comment.
// Key in the threads map: "path:line" (or just "path" when Line=0).
type NoteThread struct {
	Path    string
	Line    int
	DiffPos int // diff position for GitHub inline comments
	Visible bool
	Messages []Message
}

// ThreadKey returns the map key for a thread.
func ThreadKey(path string, line int) string {
	if line == 0 {
		return path
	}
	return fmt.Sprintf("%s:%d", path, line)
}

// HasLocal returns true if the thread has at least one local (non-GH) message.
func (t NoteThread) HasLocal() bool {
	for _, m := range t.Messages {
		if !m.FromGH {
			return true
		}
	}
	return false
}

// ── parse / fetch ─────────────────────────────────────────────────────────────

func Parse(ref, gitRoot string) (PR, error) {
	var pr PR
	urlRe := regexp.MustCompile(`github\.com[/:]([^/]+)/([^/]+)/pull/(\d+)`)
	if m := urlRe.FindStringSubmatch(ref); m != nil {
		pr.Owner = m[1]
		pr.Repo = strings.TrimSuffix(m[2], ".git")
		n, _ := strconv.Atoi(m[3])
		pr.Number = n
	} else if n, err := strconv.Atoi(strings.TrimSpace(ref)); err == nil {
		pr.Number = n
		owner, repo, err := inferRepo(gitRoot)
		if err != nil {
			return pr, fmt.Errorf("--pr given a number but cannot infer repo: %w", err)
		}
		pr.Owner = owner
		pr.Repo = repo
	} else {
		return pr, fmt.Errorf("cannot parse PR ref %q (expected number or GitHub URL)", ref)
	}
	return pr, nil
}

func Fetch(pr *PR) error {
	type apiPR struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	out, err := ghRun("api", fmt.Sprintf("repos/%s/%s/pulls/%d",
		pr.Owner, pr.Repo, pr.Number))
	if err != nil {
		return fmt.Errorf("gh api: %w (is gh authed? run: gh auth login)", err)
	}
	var data apiPR
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return fmt.Errorf("parse PR response: %w", err)
	}
	pr.Title = data.Title
	pr.Body = data.Body
	pr.HeadSHA = data.Head.SHA
	return nil
}

func Files(pr PR) ([]string, error) {
	out, err := ghRun("pr", "diff", strconv.Itoa(pr.Number),
		"--repo", fmt.Sprintf("%s/%s", pr.Owner, pr.Repo),
		"--name-only")
	if err != nil {
		return nil, fmt.Errorf("gh pr diff: %w", err)
	}
	var files []string
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

func Diff(pr PR) (string, error) {
	out, err := ghRun("pr", "diff", strconv.Itoa(pr.Number),
		"--repo", fmt.Sprintf("%s/%s", pr.Owner, pr.Repo))
	if err != nil {
		return "", fmt.Errorf("gh pr diff: %w", err)
	}
	return out, nil
}

func FileDiff(fullDiff, path string) string {
	sections := strings.Split(fullDiff, "\ndiff --git ")
	for i, s := range sections {
		if i == 0 {
			s = strings.TrimPrefix(s, "diff --git ")
		}
		header := strings.SplitN(s, "\n", 2)[0]
		if strings.Contains(header, " b/"+path) {
			return "diff --git " + s
		}
	}
	return ""
}

// Comments fetches existing PR review comments and returns them as NoteThreads
// (one thread per path:line, with FromGH=true messages).
func Comments(pr PR) (map[string]*NoteThread, error) {
	out, err := ghRun("api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/comments?per_page=100",
			pr.Owner, pr.Repo, pr.Number))
	if err != nil {
		return nil, fmt.Errorf("gh api comments: %w", err)
	}

	type apiComment struct {
		Path             string    `json:"path"`
		Line             int       `json:"line"`
		OriginalLine     int       `json:"original_line"`
		Body             string    `json:"body"`
		Position         int       `json:"position"`
		OriginalPosition int       `json:"original_position"`
		CreatedAt        time.Time `json:"created_at"`
		User             struct {
			Login string `json:"login"`
		} `json:"user"`
	}

	var raw []apiComment
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}

	threads := make(map[string]*NoteThread)
	for _, c := range raw {
		ln := c.Line
		if ln == 0 {
			ln = c.OriginalLine
		}
		pos := c.Position
		if pos == 0 {
			pos = c.OriginalPosition
		}
		key := ThreadKey(c.Path, ln)
		if _, ok := threads[key]; !ok {
			threads[key] = &NoteThread{
				Path:    c.Path,
				Line:    ln,
				DiffPos: pos,
				Visible: true, // GH threads start visible
			}
		}
		threads[key].Messages = append(threads[key].Messages, Message{
			Author:    c.User.Login,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
			FromGH:    true,
		})
	}
	return threads, nil
}

// ── publish ───────────────────────────────────────────────────────────────────

// Publish posts all local messages in threads as a PR review.
func Publish(pr PR, threads map[string]*NoteThread, fullDiff string) error {
	type inlineComment struct {
		Path     string `json:"path"`
		Position int    `json:"position"`
		Body     string `json:"body"`
	}
	type reviewRequest struct {
		CommitID string          `json:"commit_id"`
		Body     string          `json:"body"`
		Event    string          `json:"event"`
		Comments []inlineComment `json:"comments"`
	}

	var bodyParts []string
	var inline []inlineComment

	for _, t := range threads {
		// collect only local messages
		var localMsgs []string
		for _, m := range t.Messages {
			if !m.FromGH {
				localMsgs = append(localMsgs, m.Body)
			}
		}
		if len(localMsgs) == 0 {
			continue
		}
		body := strings.Join(localMsgs, "\n\n---\n\n")

		if t.Line > 0 {
			pos := t.DiffPos
			if pos == 0 {
				pos = DiffPosition(fullDiff, t.Path, t.Line)
			}
			if pos > 0 {
				inline = append(inline, inlineComment{
					Path:     t.Path,
					Position: pos,
					Body:     body,
				})
				continue
			}
		}
		bodyParts = append(bodyParts, fmt.Sprintf("**%s**\n\n%s", t.Path, body))
	}

	req := reviewRequest{
		CommitID: pr.HeadSHA,
		Body:     strings.Join(bodyParts, "\n\n---\n\n"),
		Event:    "COMMENT",
		Comments: inline,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return publishViaStdin(pr, payload)
}

func DiffPosition(fullDiff, path string, fileLine int) int {
	fileDiff := FileDiff(fullDiff, path)
	if fileDiff == "" {
		return 0
	}
	pos := 0
	for _, line := range strings.Split(fileDiff, "\n") {
		if strings.HasPrefix(line, "@@") {
			pos++
			continue
		}
		if strings.HasPrefix(line, "-") {
			pos++
			continue
		}
		if strings.HasPrefix(line, "\\") {
			continue
		}
		if strings.HasPrefix(line, "+") || len(line) > 0 {
			pos++
		}
	}
	return 1
}

// ── helpers ───────────────────────────────────────────────────────────────────

func publishViaStdin(pr PR, payload []byte) error {
	c := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", pr.Owner, pr.Repo, pr.Number),
		"--method", "POST",
		"--input", "-")
	c.Stdin = strings.NewReader(string(payload))
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("publish review: %w\n%s", err, out)
	}
	return nil
}

func ghRun(args ...string) (string, error) {
	c := exec.Command("gh", args...)
	out, err := c.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

func inferRepo(gitRoot string) (owner, repo string, err error) {
	c := exec.Command("gh", "repo", "view", "--json", "owner,name")
	c.Dir = gitRoot
	out, err := c.Output()
	if err != nil {
		return "", "", fmt.Errorf("gh repo view: %w", err)
	}
	var data struct {
		Owner struct{ Login string } `json:"owner"`
		Name  string                 `json:"name"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return "", "", err
	}
	return data.Owner.Login, data.Name, nil
}
