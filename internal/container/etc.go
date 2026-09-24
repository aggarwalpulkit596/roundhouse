package container

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/fsutil"
)

// ResolveUser turns an image USER string ("", "1000", "1000:1000", "app",
// "app:staff") into numeric ids by reading the *container's* /etc/passwd and
// /etc/group. The host's user database is irrelevant inside the container.
func ResolveUser(root, user string) (uid, gid uint32, groups []uint32, err error) {
	if user == "" {
		return 0, 0, nil, nil
	}
	u, g, hasGroup := strings.Cut(user, ":")
	passwd := readColonFile(root, "/etc/passwd")
	if n, err := strconv.ParseUint(u, 10, 32); err == nil {
		uid = uint32(n)
		gid = uid
		for _, f := range passwd {
			if len(f) > 3 && f[2] == u {
				if pg, err := strconv.ParseUint(f[3], 10, 32); err == nil {
					gid = uint32(pg)
				}
			}
		}
	} else {
		found := false
		for _, f := range passwd {
			if len(f) > 3 && f[0] == u {
				a, _ := strconv.ParseUint(f[2], 10, 32)
				b, _ := strconv.ParseUint(f[3], 10, 32)
				uid, gid, found = uint32(a), uint32(b), true
				break
			}
		}
		if !found {
			return 0, 0, nil, fmt.Errorf("user %q not found in the image's /etc/passwd", u)
		}
	}
	groupsFile := readColonFile(root, "/etc/group")
	if hasGroup {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			gid = uint32(n)
		} else {
			found := false
			for _, f := range groupsFile {
				if len(f) > 2 && f[0] == g {
					n, _ := strconv.ParseUint(f[2], 10, 32)
					gid, found = uint32(n), true
				}
			}
			if !found {
				return 0, 0, nil, fmt.Errorf("group %q not found in the image's /etc/group", g)
			}
		}
	}
	// Supplementary groups: every group that lists the user by name.
	name := u
	for _, f := range passwd {
		if len(f) > 2 && f[2] == strconv.FormatUint(uint64(uid), 10) {
			name = f[0]
		}
	}
	for _, f := range groupsFile {
		if len(f) < 4 {
			continue
		}
		for _, member := range strings.Split(f[3], ",") {
			if member == name {
				if n, err := strconv.ParseUint(f[2], 10, 32); err == nil && uint32(n) != gid {
					groups = append(groups, uint32(n))
				}
			}
		}
	}
	return uid, gid, groups, nil
}

func homeFor(root string, uid uint32) string {
	for _, f := range readColonFile(root, "/etc/passwd") {
		if len(f) > 5 && f[2] == strconv.FormatUint(uint64(uid), 10) {
			return f[5]
		}
	}
	if uid == 0 {
		return "/root"
	}
	return "/"
}

func readColonFile(root, name string) [][]string {
	p, err := fsutil.SecureJoin(root, name)
	if err != nil {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out
}

// writeEtc writes /etc/hostname, /etc/hosts and /etc/resolv.conf into the
// container's writable layer. Docker bind-mounts these from outside instead
// so it can update them live; writing them is simpler and good enough here.
func writeEtc(root string, rec *Record, dns []string, extra map[string]string) error {
	if rec.Network == NetHost {
		// Share the host's view.
		for _, f := range []string{"/etc/hosts", "/etc/resolv.conf"} {
			if b, err := os.ReadFile(f); err == nil {
				if err := writeInRoot(root, f, b); err != nil {
					return err
				}
			}
		}
		return nil
	}
	host := rec.Spec.Hostname
	if err := writeInRoot(root, "/etc/hostname", []byte(host+"\n")); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	if rec.IP != "" {
		fmt.Fprintf(&b, "%s\t%s\n", rec.IP, host)
	} else {
		fmt.Fprintf(&b, "127.0.1.1\t%s\n", host)
	}
	names := make([]string, 0, len(extra))
	for n := range extra {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s\t%s\n", extra[n], n)
	}
	if err := writeInRoot(root, "/etc/hosts", []byte(b.String())); err != nil {
		return err
	}
	if rec.Network == NetNone {
		return nil
	}
	if len(dns) == 0 {
		dns = hostNameservers()
	}
	var r strings.Builder
	for _, ns := range dns {
		fmt.Fprintf(&r, "nameserver %s\n", ns)
	}
	return writeInRoot(root, "/etc/resolv.conf", []byte(r.String()))
}

// HostNameservers returns the resolvers containers should use by default.
func HostNameservers() []string { return hostNameservers() }

// hostNameservers copies the host's resolvers, skipping loopback ones
// (systemd-resolved's 127.0.0.53 is unreachable from another netns).
func hostNameservers() []string {
	var out []string
	if f, err := os.Open("/etc/resolv.conf"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) == 2 && fs[0] == "nameserver" && !strings.HasPrefix(fs[1], "127.") && fs[1] != "::1" {
				out = append(out, fs[1])
			}
		}
	}
	if len(out) == 0 {
		out = []string{"1.1.1.1", "8.8.8.8"}
	}
	return out
}

// writeInRoot replaces name inside root. The parent is resolved securely and
// an existing entry (often a symlink into /run) is removed rather than
// followed.
func writeInRoot(root, name string, b []byte) error {
	dir, err := fsutil.SecureJoin(root, filepath.Dir(name))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, filepath.Base(name))
	_ = os.Remove(p)
	return os.WriteFile(p, b, 0o644)
}
