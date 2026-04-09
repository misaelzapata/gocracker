Output assets/demos/05-compose.mp4
Output assets/demos/05-compose.gif

Set Shell "bash"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 16
Set Width 1400
Set Height 800
Set Padding 25
Set Margin 20
Set MarginFill "#000000"
Set WindowBar Colorful
Set WindowBarSize 40
Set BorderRadius 10
Set Framerate 30
Set CursorBlink false

Set Theme { "name": "gocracker Night", "black": "#0a0e14", "red": "#f7768e", "green": "#9ece6a", "yellow": "#e0af68", "blue": "#7aa2f7", "purple": "#bb9af7", "cyan": "#73daca", "white": "#c0caf5", "brightBlack": "#565f89", "brightRed": "#f7768e", "brightGreen": "#9ece6a", "brightYellow": "#e0af68", "brightBlue": "#7aa2f7", "brightPurple": "#bb9af7", "brightCyan": "#73daca", "brightWhite": "#c0caf5", "background": "#0a0e14", "foreground": "#a9b1d6", "selectionBackground": "#283457", "cursorAccent": "#a9b1d6", "cursor": "#c0caf5" }

Hide
Type@0ms `cd ~/Desktop/projects/gocracker && export PS1='\[\033[1;34m\]gocracker\[\033[0m\] \$ '`
Enter
Sleep 100ms
Type@0ms `clear`
Enter
Sleep 300ms
Show

Sleep 800ms
Set TypingSpeed 55ms
Type "# Docker Compose: Postgres + Todo app — each service is a microVM"
Enter
Sleep 1000ms

# Show the compose file first
Set TypingSpeed 40ms
Type "cat tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml"
Sleep 200ms
Enter
Sleep 5s

# Launch compose in background, then test from the same terminal
Set TypingSpeed 40ms
Type "sudo env GOCRACKER_KERNEL=./artifacts/kernels/gocracker-guest-standard-vmlinux ./gocracker compose --file tests/manual-smoke/fixtures/compose-todo-postgres/docker-compose.yml --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --mem 256 --jailer off &"
Sleep 500ms
Enter

# Wait for stack to come up
Sleep 50s

# Test the API
Set TypingSpeed 50ms
Type "# Test: create a TODO via the app VM talking to the Postgres VM"
Enter
Sleep 1s

Set TypingSpeed 40ms
Type "curl -fsS http://127.0.0.1:18081/health"
Sleep 300ms
Enter
Sleep 3s

Set TypingSpeed 40ms
Type `curl -fsS -X POST -H 'Content-Type: application/json' -d '{"title":"buy milk"}' http://127.0.0.1:18081/api/todos`
Sleep 300ms
Enter
Sleep 3s

Set TypingSpeed 40ms
Type "curl -fsS http://127.0.0.1:18081/api/todos | python3 -m json.tool"
Sleep 300ms
Enter
Sleep 5s
