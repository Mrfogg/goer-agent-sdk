package base

import (
	"strings"
)

// MsgSummary is the summary of one run's output messages, ready to be persisted
// and handed back to the caller.
type MsgSummary struct {
	// Answer is the final answer; when visible content exists it wins.
	Answer string
	// HtmlContent is the dashboard payload, when the run produced one.
	HtmlContent string
	// VisibleContent is the user-visible content of the run: markdown,
	// task_completed records, chart attachments and stop notices concatenated.
	VisibleContent string
}

// SummarizeMessages extracts the final answer, the visible content and the
// dashboard HTML from the messages of one run. Frontends streaming the run
// events and backends persisting the conversation share this logic so the stored
// record matches what the user saw.
func SummarizeMessages(msgs []Msg) MsgSummary {
	var summary MsgSummary
	parts := make([]string, 0, len(msgs))
	appendPart := func(content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		parts = append(parts, content)
		summary.VisibleContent = strings.Join(parts, "")
	}

	for _, msg := range msgs {
		switch msg.Type {
		case MsgTypeRunDone:
			summary.Answer = strings.TrimSpace(msg.Content)
			if answer, ok := msg.Data["answer"].(string); ok && strings.TrimSpace(answer) != "" {
				summary.Answer = strings.TrimSpace(answer)
			}
		case MsgTypeRunStopped:
			appendPart(msg.Content)
		case MsgTypeChartResult:
			appendPart(ChartAttachmentContent(msg))
		case MsgTypeTaskCompleted:
			appendPart(FormatTaskCompletedRecord(msg))
		case MsgTypeMarkdown:
			appendPart(msg.Content)
		case MsgTypeDashboardHTML:
			summary.HtmlContent = msg.Content
		}
	}
	if strings.TrimSpace(summary.VisibleContent) != "" {
		summary.Answer = summary.VisibleContent
	}
	return summary
}
