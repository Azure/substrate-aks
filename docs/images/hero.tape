# Recorded at twice the size of common.tape, with the same terminal layout.
Set Shell "bash"
Set FontFamily "DejaVu Sans Mono"
Set FontSize 30
Set Width 2480
Set Height 1560
Set Padding 36
Set Framerate 12
Set TypingSpeed 40ms
Set Theme { "name": "substrate", "background": "#18181b", "foreground": "#e4e4e7", "cursor": "#a78bfa", "selection": "#3f3f46", "black": "#18181b", "red": "#f87171", "green": "#4ade80", "yellow": "#facc15", "blue": "#60a5fa", "magenta": "#c084fc", "cyan": "#22d3ee", "white": "#e4e4e7", "brightBlack": "#71717a", "brightRed": "#fca5a5", "brightGreen": "#86efac", "brightYellow": "#fde047", "brightBlue": "#93c5fd", "brightMagenta": "#d8b4fe", "brightCyan": "#67e8f9", "brightWhite": "#fafafa" }
Env TERM "xterm-256color"
Env COLORTERM "truecolor"
Env TOUR "fresh"
Env TOUR_DEMO "1"
Output hero.mp4
Hide
Type "clear && bin/substrate-aks -test.run TestTour 2>/dev/null"
Enter
Wait+Screen@20s /Quick start/
Show
Sleep 8s
Hide
Ctrl+C
Ctrl+C
