Output assets/demos/02-interactive-tty.mp4
Output assets/demos/02-interactive-tty.gif

Set Shell "bash"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 16
Set Width 1200
Set Height 750
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
# Create a wrapper that runs gocracker and strips CSI sequences from output
Type@0ms `cat > /tmp/gc-clean.sh << 'GCEOF'`
Enter
Type@0ms `#!/bin/bash`
Enter
Type@0ms `exec sudo ./gocracker "$@" 2>&1 | sed -u 's/\x1b\[[0-9;?]*[A-Za-z]//g'`
Enter
Type@0ms `GCEOF`
Enter
Type@0ms `chmod +x /tmp/gc-clean.sh`
Enter
Sleep 100ms
Type@0ms `clear`
Enter
Sleep 300ms
Show

Sleep 1000ms

Set TypingSpeed 65ms
Type "# Inter"
Set TypingSpeed 50ms
Type "active console"
Sleep 200ms
Type " — shell inside a microVM"
Enter
Sleep 1500ms

# Natural typing with pauses and speed variation
Set TypingSpeed 80ms
Type "sudo"
Sleep 300ms
Set TypingSpeed 60ms
Type " ./gocracker"
Sleep 250ms
Type " run"
Sleep 350ms
Set TypingSpeed 55ms
Type " --image"
Sleep 200ms
Type " alpi"
Set TypingSpeed 35ms
Type "ne:3.20"
Sleep 400ms
Set TypingSpeed 55ms
Type " --kernel"
Sleep 200ms
Type " ./art"
Set TypingSpeed 10ms
Type "ifacts/kernels/gocracker-guest-standard-vmlinux"
Sleep 400ms
Set TypingSpeed 55ms
Type " --wait --tty off --jailer off"

# Swap the command for the clean wrapper version
Hide
Ctrl+U
Type@0ms `bash /tmp/gc-clean.sh run --image alpine:3.20 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --cmd 'sh' --env 'PS1=/ # ' --env TERM=dumb --env ENV=/dev/null --wait --tty off --jailer off`
Show
Sleep 600ms
Enter

# Wait for kernel boot
Sleep 12s

# Guest shell commands — natural typing with think-pauses
Set TypingSpeed 80ms
Type "ech"
Set TypingSpeed 60ms
Type "o hello"
Sleep 150ms
Type " from inside"
Sleep 120ms
Type " the VM"
Sleep 400ms
Enter
Sleep 2500ms

Set TypingSpeed 70ms
Type "una"
Set TypingSpeed 50ms
Type "me -a"
Sleep 400ms
Enter
Sleep 2500ms

Set TypingSpeed 70ms
Type "cat"
Sleep 200ms
Type " /etc/os"
Set TypingSpeed 40ms
Type "-release"
Sleep 200ms
Type " | head -4"
Sleep 400ms
Enter
Sleep 2500ms

Set TypingSpeed 70ms
Type "ls"
Sleep 200ms
Type " /dev/vda"
Sleep 400ms
Enter
Sleep 2000ms

Set TypingSpeed 80ms
Type "exit"
Sleep 500ms
Enter
Sleep 5s
