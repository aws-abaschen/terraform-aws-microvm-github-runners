package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// VerifySignature checks X-Hub-Signature-256 in constant time.
func VerifySignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if len(secret) == 0 || !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// WorkflowJobEvent is the subset of the workflow_job webhook we use.
type WorkflowJobEvent struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID           int64     `json:"id"`
		RunID        int64     `json:"run_id"`
		RunAttempt   int       `json:"run_attempt"`
		Name         string    `json:"name"`
		WorkflowName string    `json:"workflow_name"`
		HeadSHA      string    `json:"head_sha"`
		HeadBranch   string    `json:"head_branch"`
		Labels       []string  `json:"labels"`
		RunnerID     int64     `json:"runner_id"`
		RunnerName   string    `json:"runner_name"`
		Conclusion   string    `json:"conclusion"`
		CreatedAt    time.Time `json:"created_at"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}
