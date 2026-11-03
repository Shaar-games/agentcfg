# Browse a realistic-but-inert local inventory using the app itself.
Output docs/demo/agentcfg-list.gif
Set Shell bash
Set FontFamily "JetBrains Mono"
Set FontSize 22
Set Width 1080
Set Height 610
Set Theme "Catppuccin Mocha"
Set Framerate 12
Set TypingSpeed 45 ms
Set CursorBlink false
Set WindowBar Colorful

Env HOME "/vhs/.demo/home"
Env PATH "/vhs/.demo/bin:/usr/local/bin:/usr/bin:/bin"
Env TERM "xterm-256color"

Type "agentcfg list"
Enter
Wait+Screen /Inventory/
Sleep 800 ms
Down 2
Enter
Wait+Screen /Back to inventory/
Sleep 500 ms
Enter
Wait+Screen /Skill: react-best-practices/
Sleep 1 s
Enter
Sleep 250 ms
Ctrl+C
Sleep 300 ms
Down
Enter
Wait+Screen /MCP servers/
Sleep 500 ms
Enter
Wait+Screen /MCP: context7/
Sleep 1 s
Enter
Sleep 300 ms
Ctrl+C
Sleep 300 ms
Ctrl+C
Sleep 500 ms
