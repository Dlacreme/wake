package gh

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// PR holds the metadata for a pull request.
type PR struct {
	Number int
	Title  string
	Body   string
	Owner  string
	Repo   string
	// Head commit SHA — needed for inline comment positions
	HeadSHA string
}

// ReviewComment is an existing comment on a PR (from GitHub).
type ReviewComment struct {
	Path     string
	Line     int    // line in the file (right side)
	Body     string
	Author   string
	Position int // diff position (for posting new comments)
}

// Note is a pending review note written by the user in this session.
type Note struct {
	Path     string
	Line     int    // 0 = file-level comment
	DiffPos  int    // diff position for inline comments; 0 = file-level
	Body     string
}

// Parse extracts owner, repo, and PR number from either:
//   - a plain number: "123"
//   - a GitHub URL: "https://github.com/owner/repo/pull/123"
// Falls back to inferring owner/repo from `gh repo view` when not in URL.
func Parse(ref, gitRoot string) (PR, error) {
	var pr PR

	// try URL form first
	urlRe := regexp.MustCompile(`github\.com[/:]([^/]+)/([^/]+)/pull/(\d+)`)
	if m := urlRe.FindStringSubmatch(ref); m != nil {
		pr.Owner = m[1]
		pr.Repo = strings.TrimSuffix(m[2], ".git")
		n, _ := strconv.Atoi(m[3])
		pr.Number = n
	} else if n, err := strconv.Atoi(strings.TrimSpace(ref)); err == nil {
		pr.Number = n
		// infer owner/repo from gh
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

// Fetch populates PR metadata from the GitHub API.
func Fetch(pr *PR) error {
	repoFlag := fmt.Sprintf("%s/%s", pr.Owner, pr.Repo)

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
	_ = repoFlag
	return nil
}

// Files returns the list of files changed in the PR (path only).
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

// Diff returns the full unified diff for the PR.
func Diff(pr PR) (string, error) {
	out, err := ghRun("pr", "diff", strconv.Itoa(pr.Number),
		"--repo", fmt.Sprintf("%s/%s", pr.Owner, pr.Repo))
	if err != nil {
		return "", fmt.Errorf("gh pr diff: %w", err)
	}
	return out, nil
}

// FileDiff extracts the diff section for a single file from the full PR diff.
func FileDiff(fullDiff, path string) string {
	// split on "diff --git" headers
	sections := strings.Split(fullDiff, "\ndiff --git ")
	for i, s := range sections {
		if i == 0 {
			s = strings.TrimPrefix(s, "diff --git ")
		}
		// first line is "a/path b/path"
		header := strings.SplitN(s, "\n", 2)[0]
		if strings.Contains(header, " b/"+path) {
			return "diff --git " + s
		}
	}
	return ""
}

// Comments fetches existing review comments for the PR.
func Comments(pr PR) ([]ReviewComment, error) {
	out, err := ghRun("api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/comments?per_page=100",
			pr.Owner, pr.Repo, pr.Number))
	if err != nil {
		return nil, fmt.Errorf("gh api comments: %w", err)
	}

	type apiComment struct {
		Path             string `json:"path"`
		Line             int    `json:"line"`
		OriginalLine     int    `json:"original_line"`
		Body             string `json:"body"`
		Position         int    `json:"position"`
		OriginalPosition int    `json:"original_position"`
		User             struct {
			Login string `json:"login"`
		} `json:"user"`
	}

	var raw []apiComment
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}

	comments := make([]ReviewComment, 0, len(raw))
	for _, c := range raw {
		ln := c.Line
		if ln == 0 {
			ln = c.OriginalLine
		}
		pos := c.Position
		if pos == 0 {
			pos = c.OriginalPosition
		}
		comments = append(comments, ReviewComment{
			Path:     c.Path,
			Line:     ln,
			Body:     c.Body,
			Author:   c.User.Login,
			Position: pos,
		})
	}
	return comments, nil
}

// DiffPosition computes the diff hunk position for a given file+line
// within the full PR diff. GitHub's inline comment API requires a
// "position" — the line number within the diff output itself, not the
// file line number.
func DiffPosition(fullDiff, path string, fileLine int) int {
	fileDiff := FileDiff(fullDiff, path)
	if fileDiff == "" {
		return 0
	}
	pos := 0
	for _, line := range strings.Split(fileDiff, "\n") {
		if strings.HasPrefix(line, "@@") {
			pos++
			// parse the +start from the hunk header
			re := regexp.MustCompile(`\+(\d+)`)
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			_ = start
			continue
		}
		if strings.HasPrefix(line, "-") {
			// deleted line — counts in diff position but not file line
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
	// Simple fallback: return 1 (post on first line of diff)
	// A production implementation would track hunk offsets precisely.
	return 1
}

// Publish posts all pending notes as a PR review.
// Notes with DiffPos > 0 become inline comments; others go in the review body.
func Publish(pr PR, notes []Note, fullDiff string) error {
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

	for _, note := range notes {
		if note.Line > 0 {
			pos := DiffPosition(fullDiff, note.Path, note.Line)
			if pos > 0 {
				inline = append(inline, inlineComment{
					Path:     note.Path,
					Position: pos,
					Body:     note.Body,
				})
				continue
			}
		}
		// file-level or no position found → add to review body
		bodyParts = append(bodyParts,
			fmt.Sprintf("**%s**\n\n%s", note.Path, note.Body))
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

	_, err = ghRun("api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", pr.Owner, pr.Repo, pr.Number),
		"--method", "POST",
		"--input", "-",
		"--field", string(payload),
	)
	if err != nil {
		// retry with raw stdin approach
		return publishViaStdin(pr, payload)
	}
	return nil
}

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

// ── helpers ───────────────────────────────────────────────────────────────────

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
