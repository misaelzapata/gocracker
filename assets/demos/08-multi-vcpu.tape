Output assets/demos/08-multi-vcpu.mp4
Output assets/demos/08-multi-vcpu.gif

Set Shell "bash"
Set FontFamily "JetBrainsMono Nerd Font"
Set FontSize 16
Set Width 1200
Set Height 700
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
Type "# SMP: boot with 4 vCPUs and 512 MB RAM"
Enter
Sleep 1200ms

Set TypingSpeed 40ms
Type `sudo ./gocracker run --image alpine:3.20 --kernel ./artifacts/kernels/gocracker-guest-standard-vmlinux --cpus 4 --mem 512 --cmd 'sh -c "echo CPUs: $(nproc) && free -m"' --wait --tty off --jailer off`
Sleep 500ms
Enter

Sleep 18s
