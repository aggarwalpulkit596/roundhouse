package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/builder"
	"github.com/aggarwalpulkit596/capital-lab/roundhouse/internal/registry"
)

func init() {
	register("build", "Build & ship", "Build an image from a Railfile/Dockerfile", cmdBuild)
	register("tag", "Images", "Give an image another name", cmdTag)
	register("push", "Build & ship", "Upload an image to a registry", cmdPush)
	register("registry", "Build & ship", "Run an OCI distribution registry", cmdRegistry)
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	tag := fs.String("t", "", "name for the image (default localhost/<dir>:latest)")
	file := fs.String("f", "", "Railfile path (default: CONTEXT/Railfile, then CONTEXT/Dockerfile)")
	target := fs.String("target", "", "build up to this stage")
	noCache := fs.Bool("no-cache", false, "ignore the step cache")
	netMode := fs.String("network", "host", "network for RUN steps: host or none")
	epoch := fs.String("source-date-epoch", os.Getenv("SOURCE_DATE_EPOCH"), "clamp timestamps for reproducible layers (unix seconds)")
	var buildArgs stringList
	fs.Var(&buildArgs, "build-arg", "KEY=VALUE for ARG (repeatable)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: rh build [flags] CONTEXT"); fs.PrintDefaults() }
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return exitError(2)
	}
	if *netMode != "host" && *netMode != "none" {
		return fmt.Errorf("--network must be host or none")
	}
	m, err := manager()
	if err != nil {
		return err
	}
	o := builder.Options{
		ContextDir: fs.Arg(0), File: *file, Tag: *tag, Target: *target,
		NoCache: *noCache, Network: *netMode, Out: os.Stdout, BuildArgs: map[string]string{},
	}
	for _, kv := range buildArgs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid --build-arg %q", kv)
		}
		o.BuildArgs[k] = v
	}
	if *epoch != "" {
		sec, err := strconv.ParseInt(*epoch, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid SOURCE_DATE_EPOCH %q", *epoch)
		}
		t := time.Unix(sec, 0).UTC()
		o.Epoch = &t
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b := &builder.Builder{Store: m.Images, Dir: filepath.Join(m.Root, "build")}
	res, err := b.Build(ctx, o)
	if err != nil {
		return err
	}
	fmt.Printf("\nbuilt %s (%s) in %s — %d steps, %d cached\n", familiar(res.Image.Name), res.Image.Config.Short(), res.Duration.Round(time.Millisecond), res.Steps, res.Cached)
	return nil
}

func cmdTag(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: rh tag SOURCE TARGET")
	}
	m, err := manager()
	if err != nil {
		return err
	}
	im, err := m.Images.TagAs(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Println(familiar(im.Name))
	return nil
}

func cmdPush(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: rh push IMAGE [DESTINATION]")
	}
	m, err := manager()
	if err != nil {
		return err
	}
	src, dst := args[0], args[0]
	if len(args) == 2 {
		dst = args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	start := time.Now()
	if err := m.Images.Push(ctx, src, dst, func(f string, a ...any) { fmt.Printf(f+"\n", a...) }); err != nil {
		return err
	}
	fmt.Printf("pushed %s in %s\n", dst, time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdRegistry(args []string) error {
	fs := flag.NewFlagSet("registry", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:5000", "address to listen on")
	root := fs.String("root", "", "storage directory (default RH_ROOT/registry)")
	quiet := fs.Bool("q", false, "do not log requests")
	_ = fs.Parse(args)
	if *root == "" {
		*root = filepath.Join(stateRoot(), "registry")
	}
	srv, err := registry.New(*root)
	if err != nil {
		return err
	}
	if *quiet {
		srv.Logf = nil
	}
	fmt.Printf("OCI registry on http://%s (storage %s)\n", *addr, *root)
	hs := &http.Server{Addr: *addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
