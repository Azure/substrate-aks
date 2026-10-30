package main

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// setupLogTailBytes bounds how much of the private command log the logs view
// reads per refresh; the full file stays on disk.
const setupLogTailBytes = 64 << 10

// refreshLog reloads the logs view from the current command's private log.
func (m *setupWorkspace) refreshLog() {
	if m.logPath == "" {
		m.logTail = ""
		return
	}
	text, err := readSetupLogTail(m.logPath, setupLogTailBytes)
	if err != nil {
		m.logTail = m.st.warn.Render("Could not read the diagnostic log: " + safeSetupText(err.Error()))
		return
	}
	m.logTail = m.styleLog(text)
}

func readSetupLogTail(path string, limit int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	offset := max(0, info.Size()-limit)
	data := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(data, offset); err != nil && err != io.EOF {
		return "", err
	}
	text := string(data)
	if offset > 0 {
		// Start at a line boundary so a partial command or escape never leads.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		text = "[Earlier output omitted; the full log is on disk]\n" + text
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return safeSetupText(text), nil
}

// styleLog makes the transcript scannable: steps stand out, commands are
// readable shell lines, and subprocess output recedes.
func (m *setupWorkspace) styleLog(text string) string {
	st := m.st
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "[step] "):
			lines[i] = st.bold.Render("▸ " + strings.TrimPrefix(line, "[step] "))
		case strings.HasPrefix(line, "$ "):
			lines[i] = st.soft.Render("$ " + setupShellLine(strings.TrimPrefix(line, "$ ")))
		case strings.HasPrefix(line, "[failed]"), strings.HasPrefix(line, "[error]"):
			lines[i] = st.fail.Render(line)
		case strings.HasPrefix(line, "[complete]"):
			lines[i] = st.ok.Render(line)
		default:
			lines[i] = st.muted.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

// setupShellLine turns the runner's fully quoted argv ("az" "group" ...) into
// a familiar shell line, quoting only arguments that need it.
func setupShellLine(quoted string) string {
	var args []string
	rest := strings.TrimSpace(quoted)
	for rest != "" {
		prefix, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return quoted
		}
		arg, err := strconv.Unquote(prefix)
		if err != nil {
			return quoted
		}
		if arg == "" || strings.ContainsAny(arg, " \t\"'\\$`|&;<>(){}*?[]#~") {
			arg = strconv.Quote(arg)
		}
		args = append(args, arg)
		rest = strings.TrimSpace(rest[len(prefix):])
	}
	return strings.Join(args, " ")
}
