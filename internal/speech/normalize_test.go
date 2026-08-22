package speech

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeKeepsProseAndUsesOneCombinedScreenCue(t *testing.T) {
	input := `# Result

This is the **natural explanation** with [helpful docs](https://example.com/docs).

Run ` + "`go test ./...`" + ` after making the change.

` + "```go" + `
func main() {
    println("not spoken")
}
` + "```" + `

| Name | Value |
| --- | --- |
| alpha | beta |

` + "```json" + `
{"secret":"not spoken"}
` + "```" + `

See https://example.com/raw for more.

The final prose is spoken.`

	got := Normalize(input)
	for _, want := range []string{"natural explanation", "helpful docs", "final prose is spoken", "Refer to the code, table, structured data, and link on screen."} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized speech missing %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"func main", `{"secret"`, "alpha", "go test ./...", "https://"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("normalized speech retained %q: %q", unwanted, got)
		}
	}
	if count := strings.Count(got, "Refer to the"); count != 1 {
		t.Fatalf("screen cue count=%d speech=%q", count, got)
	}
}

func TestNormalizeCodeOnlyStillRefersToScreen(t *testing.T) {
	got := Normalize("```sh\necho hello\n```")
	if got != "Refer to the code on screen." {
		t.Fatalf("speech=%q", got)
	}
}

func TestNormalizeTopLevelJSON(t *testing.T) {
	if got := Normalize(`{"ok":true}`); got != "Refer to the structured data on screen." {
		t.Fatalf("speech=%q", got)
	}
}

func TestChunkSpeaksCompleteTextWithinRuneLimit(t *testing.T) {
	input := "First sentence is short. Second sentence contains café and more words. Third sentence finishes everything."
	chunks := Chunk(input, 35)
	if len(chunks) < 3 {
		t.Fatalf("chunks=%q", chunks)
	}
	for _, chunk := range chunks {
		if count := utf8.RuneCountInString(chunk); count > 35 {
			t.Fatalf("chunk has %d runes: %q", count, chunk)
		}
	}
	if got := strings.Join(chunks, " "); got != input {
		t.Fatalf("reconstructed=%q want=%q", got, input)
	}
}
