package buildinfo

// Version is set at build time via ldflags:
//
//	go build -ldflags "-X github.com/LNDCAI001/yesmem/internal/buildinfo.Version=..."
var Version = "dev"
