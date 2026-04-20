package embed

//go:generate sh -c "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o gocracker-toolbox-amd64 ../../../cmd/gocracker-toolbox && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o gocracker-toolbox-arm64 ../../../cmd/gocracker-toolbox"
