package api

import (
	"io"
	"strconv"
	"strings"

	"harness-go/internal/events"
)

// writeSSEFrame 按 SSE 规范写一帧：
//
//	id: <seq>
//	event: <type>
//	data: <json>
//	<空行>
//
// 注意：id 必须是 Run 内单调递增的 seq，客户端断线后用它作为 Last-Event-ID 续传。
func writeSSEFrame(w io.Writer, ev events.Event) error {
	var sb strings.Builder
	sb.WriteString("id: ")
	sb.WriteString(strconv.FormatInt(ev.Seq, 10))
	sb.WriteString("\nevent: ")
	sb.WriteString(string(ev.Type))
	sb.WriteString("\ndata: ")
	if len(ev.Payload) == 0 {
		sb.WriteString("{}")
	} else {
		sb.Write(ev.Payload)
	}
	sb.WriteString("\n\n")
	_, err := io.WriteString(w, sb.String())
	return err
}

// writeSSEComment 写心跳注释帧（保持连接活跃，不触发客户端 onmessage）。
func writeSSEComment(w io.Writer, text string) error {
	_, err := io.WriteString(w, ": "+text+"\n\n")
	return err
}
