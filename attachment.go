package base

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Product-facing message conventions used by SummarizeMessages.
//
// These helpers encode conventions of the product built on top of this SDK
// (chart attachments, task-completion records). A consumer that does not use
// them can ignore this file, or substitute its own extractors when building its
// own summary.

// ChartAttachmentContent renders the <attachments> visible content of a
// chart_result message.
func ChartAttachmentContent(msg Msg) string {
	chartID, _ := msg.Data["chart_id"].(string)
	if strings.TrimSpace(chartID) == "" {
		return ""
	}
	chartOption, ok := msg.Data["chart_option"]
	if !ok || chartOption == nil {
		return ""
	}
	attachments := map[string]any{
		chartID: map[string]any{
			"type": "chart",
			"data": chartOption,
		},
	}
	attachmentsJSON, err := json.Marshal(attachments)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(`<attachments data=%q></attachments>`, string(attachmentsJSON))
}

// FormatTaskCompletedRecord formats a task_completed message as a visible
// record.
func FormatTaskCompletedRecord(msg Msg) string {
	content := strings.TrimSpace(msg.Content)
	if len(msg.Data) == 0 {
		return content
	}
	dataJSON, err := json.Marshal(msg.Data)
	if err != nil {
		return content
	}
	if content == "" {
		return fmt.Sprintf("<%s data=%q></%s>", msg.Type, string(dataJSON), msg.Type)
	}
	return fmt.Sprintf("<%s data=%q>%s</%s>", msg.Type, string(dataJSON), content, msg.Type)
}
