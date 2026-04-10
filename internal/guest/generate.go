package guest

//go:generate sh -c "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -extldflags \"-static\"' -o init_amd64.bin ./init.go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w -extldflags \"-static\"' -o init_arm64.bin ./init.go"
