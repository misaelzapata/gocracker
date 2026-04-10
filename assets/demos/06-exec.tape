Output assets/demos/06-exec.mp4
Output assets/demos/06-exec.gif

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
# Fix cache permissions so serve can write
Type@0ms `sudo chmod -R 777 /tmp/gocracker/cache 2>/dev/null; mkdir -p /tmp/gocracker-demo-cache`
Enter
Sleep 200ms
Type@0ms `clear`
Enter
Sleep 300ms
Show

Sleep 1000ms

Set TypingSpeed 65ms
Type "# API"
Sleep 150ms
Type " exec:"
Sleep 200ms
Type " start"
Sleep 100ms
Type " serve,"
Sleep 150ms
Type " boot VM,"
Sleep 150ms
Type " exec into it"
Enter
Sleep 1500ms

# Step 1: start serve (with sudo for cache access)
Set TypingSpeed 80ms
Type "sudo"
Sleep 200ms
Set TypingSpeed 55ms
Type " ./gocracker"
Sleep 200ms
Type " serve"
Sleep 250ms
Type " --addr :18099"
Sleep 200ms
Type " --jailer off"
Sleep 200ms
Type " --cache-dir"
Sleep 150ms
Type " /tmp/gocracker-demo-cache"
Sleep 200ms
Type " &"
Sleep 500ms
Enter
Sleep 4s

# Step 2: boot a VM via API
Set TypingSpeed 65ms
Type "# Bo"
Set TypingSpeed 50ms
Type "ot a VM"
Sleep 150ms
Type " through the API"
Enter
Sleep 800ms

Set TypingSpeed 55ms
Type "curl"
Sleep 200ms
Type " -sS"
Sleep 150ms
Type " -X POST"
Sleep 200ms
Type " http://localhost:18099/run"
Sleep 200ms
Type " -H 'Content-Type: application/json'"
Sleep 250ms
Type ` -d '{"image":"alpine:3.20","kernel_path":"./artifacts/kernels/gocracker-guest-standard-vmlinux","mem_mb":256,"net":"auto","wait":true}'`
Sleep 500ms
Enter
Sleep 18s

# Step 3: list VMs
Set TypingSpeed 65ms
Type "# Li"
Set TypingSpeed 50ms
Type "st running VMs"
Enter
Sleep 800ms

Set TypingSpeed 55ms
Type "curl"
Sleep 200ms
Type " -sS"
Sleep 150ms
Type " http://localhost:18099/vms"
Sleep 200ms
Type " | python3 -m json.tool"
Sleep 200ms
Type " | head -12"
Sleep 400ms
Enter
Sleep 4s

# Save VM ID
Hide
Type@0ms `VMID=$(curl -sS http://localhost:18099/vms | python3 -c "import sys,json; print(json.load(sys.stdin)[0]['id'])")`
Enter
Sleep 1s
Show

# Step 4: exec a command
Set TypingSpeed 65ms
Type "# Ex"
Set TypingSpeed 50ms
Type "ec 'uname -a'"
Sleep 150ms
Type " inside the guest"
Sleep 150ms
Type " — no SSH, just vsock"
Enter
Sleep 800ms

Set TypingSpeed 55ms
Type "curl"
Sleep 200ms
Type " -sS"
Sleep 150ms
Type " -X POST"
Sleep 200ms
Type " http://localhost:18099/vms/$VMID/exec"
Sleep 250ms
Type " -H 'Content-Type: application/json'"
Sleep 200ms
Type ` -d '{"command":["uname","-a"]}'`
Sleep 200ms
Type " | python3 -m json.tool"
Sleep 500ms
Enter
Sleep 5s

# Step 5: another exec
Set TypingSpeed 55ms
Type "curl"
Sleep 200ms
Type " -sS"
Sleep 150ms
Type " -X POST"
Sleep 200ms
Type " http://localhost:18099/vms/$VMID/exec"
Sleep 250ms
Type " -H 'Content-Type: application/json'"
Sleep 200ms
Type ` -d '{"command":["cat","/etc/os-release"]}'`
Sleep 200ms
Type " | python3 -m json.tool"
Sleep 500ms
Enter
Sleep 6s
