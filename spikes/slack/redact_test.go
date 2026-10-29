package main

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	r := newRedactor()
	r.add("Jane Example", "<NAME>")
	in := `{"token":"abc123","user":{"id":"U0123ABCD45","name":"Jane Example"},"channel":"C09ZZZZZZ1","team":"T01ABCDEF2",` +
		`"response_url":"https:\/\/hooks.slack.com\/actions\/T01ABCDEF2\/123\/secret","email":"jane@example.com","t":"xoxb-1-2-abc","ts":"1730000000.123456"}`
	out := r.redact(in)
	for _, leak := range []string{"abc123", "U0123ABCD45", "Jane Example", "C09ZZZZZZ1", "T01ABCDEF2", "hooks.slack.com", "jane@example.com", "xoxb-"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %s", leak, out)
		}
	}
	if !strings.Contains(out, "1730000000.123456") {
		t.Errorf("ts should survive: %s", out)
	}
	if r.redact("U0123ABCD45") != r.redact("U0123ABCD45") {
		t.Error("placeholders should be stable")
	}
}
