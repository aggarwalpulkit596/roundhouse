# F3: Go for infrastructure code

> **Goal:** read and modify Roundhouse, runc, containerd and BuildKit comfortably. They are all Go, as are Docker, Kubernetes, etcd, Terraform and most cloud-native infrastructure. You do not need to be a Go expert; you need to be fluent in the parts infrastructure code uses. Day 4 of the [20-day plan](../00-study-plan.md).

## What to learn, in order

1. **Basics (day 1):** do the [Tour of Go](https://go.dev/tour/) end to end. Types, structs, methods, interfaces, slices, maps, errors, `defer`.
2. **Errors (day 1):** Go returns errors as values; every call site decides. Learn `fmt.Errorf("context: %w", err)`, `errors.Is`, `errors.As`. Roundhouse wraps errors with context everywhere (`"mount overlay: %w"`) so a failure explains itself.
3. **Concurrency (day 2):** goroutines, channels, `select`, `sync.Mutex`, `sync.WaitGroup`, `context.Context` for cancellation and timeouts. [_Go by Example_](https://gobyexample.com/) covers each in a page.
4. **Testing (day 2):** `go test`, table-driven tests, `t.TempDir()`, `httptest`, the race detector (`go test -race`). [_Learn Go with Tests_](https://quii.gitbook.io/learn-go-with-tests) is excellent practice.
5. **The standard library that infrastructure uses:** `os`, `os/exec`, `io`, `bufio`, `net`, `net/http`, `encoding/json`, `archive/tar`, `compress/gzip`, `crypto/sha256`, `syscall` and `golang.org/x/sys/unix`.

## Patterns you will see in Roundhouse (and everywhere else)

| Pattern                              | Where in Roundhouse                       | Why                                                         |
| ------------------------------------ | ----------------------------------------- | ----------------------------------------------------------- |
| Interface as a seam for testing      | `engine.Runtime`, `engine.Prober`         | Test the control loop against fakes, without root           |
| `context.Context` for cancellation   | `Store.Pull`, `Engine.Run`, HTTP handlers | Stop work when the user hits Ctrl-C or a client disconnects |
| Worker pool with bounded concurrency | `errgroup.SetLimit(4)` in `Store.Pull`    | Parallel downloads without unbounded goroutines             |
| Mutex around shared state            | `Engine.mu`                               | Many goroutines read and update services                    |
| Atomic pointer swap                  | `network.Proxy.SetBackends`               | Change routing without locks on the hot path                |
| Temp file + fsync + rename           | `image.WriteFileAtomic`, `Store.PutBlob`  | A crash never leaves a half-written file                    |
| `io.TeeReader` / `io.MultiWriter`    | hashing while unpacking or downloading    | Verify content in one pass                                  |
| Re-exec of `/proc/self/exe`          | `runtime.Start`                           | Go cannot safely `fork()` (module 1)                        |
| `runtime.LockOSThread`               | `runtime/init.go`                         | Namespaces and capabilities are per-thread                  |
| cgo constructor                      | `internal/nsenter`                        | Run C before the Go runtime starts threads                  |

## The concurrency ideas that matter most

- **Goroutines are cheap; unbounded goroutines are not.** Bound concurrency with a semaphore, a worker pool or `errgroup.SetLimit`.
- **Share memory by communicating, or lock it.** Channels for handing work around; mutexes for protecting state. Roundhouse uses both.
- **Never hold a lock during slow I/O.** The engine copies what it needs under the lock, releases it, then pulls images or stops containers. Read `sync.go` with this in mind.
- **The race detector finds real bugs.** It found two in Roundhouse's engine during development (views returning shared pointers). Always run `go test -race`.

## Lab F3

1. Tour of Go, all of it.
2. Write a tiny concurrent downloader: fetch 10 URLs with at most 3 in flight, compute each body's sha256, cancel everything on Ctrl-C (`signal.NotifyContext`). This is the core of `Store.Pull`.
3. Write a table-driven test for `image.ParseReference` cases you think are missing, and run it: `go test ./internal/image/ -run ParseReference -v`.
4. Read [`internal/engine/queue.go`](../../internal/engine/queue.go) (about 100 lines) and its tests. Then write a test that fails if `Done` forgets to re-queue a dirty key. (Break the code, see the test fail, fix it.)
5. Run `go test -race ./internal/engine/`. Then remove one `e.mu.Lock()` in `engine.go` and run it again to see what the race detector reports. Put it back.

## Resources

- [A Tour of Go](https://go.dev/tour/), [Effective Go](https://go.dev/doc/effective_go), [Go by Example](https://gobyexample.com/).
- Alan Donovan and Brian Kernighan, _The Go Programming Language_: chapters 8 and 9 (concurrency) are the best explanation in print.
- [_Learn Go with Tests_](https://quii.gitbook.io/learn-go-with-tests).
- Katherine Cox-Buday, _Concurrency in Go_ (O'Reilly), when you want depth.

## Check yourself

1. Why does Go code return errors instead of throwing exceptions, and what does `%w` do?
2. What does `context.WithTimeout` give you, and who is responsible for checking it?
3. Why must the engine not hold `e.mu` while pulling an image?
4. What does `go test -race` detect, and why does it not find every race?
5. Why is `WriteFileAtomic` temp file + fsync + rename, and what would go wrong with a plain `os.WriteFile`?
