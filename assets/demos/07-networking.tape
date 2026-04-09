Output assets/demos/07-networking.mp4
Output assets/demos/07-networking.gif

Set Shell "bash"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 16
Set Width 1200
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

Sleep 1000ms

Set TypingSpeed 65ms
Type "# Net"
Set TypingSpeed 50ms
Type "working:"
Sleep 200ms
Type " boot nginx"
Sleep 150ms
Type " with --net auto,"
Sleep 150ms
Type " curl from host"
Enter
Sleep 1500ms

# Launch nginx in background
Set TypingSpeed 80ms
Type "sudo"
Sleep 200ms
Set TypingSpeed 55ms
Type " ./gocracker"
Sleep 200ms
Type " run"
Sleep 250ms
Type " --image"
Sleep 200ms
Type " nginx:alpine"
Sleep 250ms
Type " --kernel"
Sleep 150ms
Type " ./art"
Set TypingSpeed 10ms
Type "ifacts/kernels/gocracker-guest-standard-vmlinux"
Sleep 300ms
Set TypingSpeed 55ms
Type " --net auto"
Sleep 200ms
Type " --tty off"
Sleep 200ms
Type " --jailer off"
Sleep 200ms
Type " &"
Sleep 500ms
Enter

# Wait for VM to boot and nginx to start — need enough time
Sleep 20s

# curl from host
Set TypingSpeed 65ms
Type "# VM"
Set TypingSpeed 50ms
Type " is running"
Sleep 150ms
Type " — curl nginx from host"
Enter
Sleep 800ms

Set TypingSpeed 60ms
Type "curl"
Sleep 250ms
Type " -sS"
Sleep 200ms
Type " http://172.16.0.2"
Sleep 200ms
Type " | head -8"
Sleep 500ms
Enter
Sleep 5s

# Show TAP interface
Set TypingSpeed 60ms
Type "ip"
Sleep 200ms
Type " addr show"
Sleep 200ms
Type " | grep -A2"
Sleep 200ms
Type " gc-"
Sleep 400ms
Enter
Sleep 4s

# Show NAT rule
Set TypingSpeed 60ms
Type "sudo"
Sleep 200ms
Type " iptables"
Sleep 200ms
Type " -t nat"
Sleep 200ms
Type " -L POSTROUTING -n"
Sleep 200ms
Type " | grep 172.16"
Sleep 400ms
Enter
Sleep 5s
