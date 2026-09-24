//go:build !cgo

package nsenter

// Roundhouse needs cgo: the namespace-joining code in nsenter.go is C that
// must run before the Go runtime starts (see that file). Go disables cgo
// automatically when no C compiler is installed, which would otherwise
// surface as the cryptic "build constraints exclude all Go files".
//
// Fix: sudo apt-get install -y build-essential   (then: CGO_ENABLED=1 go build ./cmd/rh)
var _ = cgo_is_disabled_install_a_C_compiler_with_apt_install_build_essential
