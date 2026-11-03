# Browse realistic-but-inert community labels through real read-only Huh Select.
# No MCP or skill is installed, started or executed by this recording.
Output docs/demo/agentcfg-list.gif
Set Shell bash
Set FontFamily "JetBrains Mono"
Set FontSize 22
Set Width 1080
Set Height 610
Set Theme "Catppuccin Mocha"
Set Framerate 12
Set TypingSpeed 110 ms
Set CursorBlink false
Set WindowBar Colorful

Env HOME "/vhs/.demo/home"
Env PATH "/vhs/.demo/bin:/usr/local/bin:/usr/bin:/bin"
Env TERM "xterm-256color"

Type "agentcfg list"
Enter
Wait+Screen /Inventory/
# Navigate directly to the Skills section, then inspect both fixture skills.
Sleep 1900 ms
Down 2
Enter
Wait+Screen /Back to inventory/
Sleep 1400 ms
Enter
Wait+Screen /Available to/
Sleep 4200 ms
# A Huh Note closes with Enter, returning to the same skill Select menu.
Enter
Wait+Screen /Back to inventory/
Sleep 1400 ms
Down
Enter
Wait+Screen /Available to/
Sleep 3000 ms
