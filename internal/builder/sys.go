package builder

import (
	"os"
	"syscall"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/network"
)

func statOwner(fi os.FileInfo) ([2]int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int{}, false
	}
	return [2]int{int(st.Uid), int(st.Gid)}, true
}

func loopbackUp(pid int) error { return network.LoopbackUp(pid) }
