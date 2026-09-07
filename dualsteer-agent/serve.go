package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	cp "example.com/dual-steer/controlplane"
)

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	contextStatus := func(id string) (ContextStatus, bool) {
		for _, c := range d.Status().Contexts {
			if c.ID == id {
				return c, true
			}
		}
		return ContextStatus{}, false
	}
	mux.HandleFunc("GET "+cp.Prefix+"/status", func(w http.ResponseWriter, r *http.Request) { write(w, http.StatusOK, d.Status()) })
	mux.HandleFunc("GET "+cp.Prefix+"/contexts/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, ok := contextStatus(r.PathValue("id"))
		if !ok {
			http.Error(w, "context not found", http.StatusNotFound)
			return
		}
		write(w, http.StatusOK, c)
	})
	mux.HandleFunc("PUT "+cp.Prefix+"/contexts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var a cp.Assignment
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			http.Error(w, "exactly one JSON object required", http.StatusBadRequest)
			return
		}
		if a.ID != r.PathValue("id") {
			http.Error(w, "path id differs from assignment id", http.StatusBadRequest)
			return
		}
		if err := a.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Put(a); err != nil {
			code := http.StatusServiceUnavailable
			if errors.Is(err, ErrAssignmentRejected) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		c, _ := contextStatus(a.ID)
		write(w, http.StatusAccepted, c)
	})
	mux.HandleFunc("DELETE "+cp.Prefix+"/contexts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Delete(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		write(w, http.StatusOK, map[string]bool{"deleted": true})
	})
	return mux
}

func runServe(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "unix:/run/dualsteer-agent.sock", "Unix socket or loopback IP:port")
	policy := fs.String("policy-map", "", "policy map pinned path or id:N")
	path := fs.String("path-map", "", "path map pinned path or id:N")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("serve accepts only named flags")
	}
	if *policy == "" || *path == "" {
		return errors.New("serve requires --policy-map and --path-map")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Serve(ctx, *listen, *policy, *path, out)
}

// Serve requires empty selected maps in this namespace. A prior crash may leave
// orphan entries: recreate the dedicated maps or explicitly delete those entries
// with the existing CLI before restart. Other namespaces are never swept.
func Serve(ctx context.Context, listen, policySelector, pathSelector string, out io.Writer) error {
	unlock, err := LockWriters()
	if err != nil {
		return err
	}
	defer unlock()
	store, err := OpenKernelMaps(policySelector, pathSelector)
	if err != nil {
		return err
	}
	defer store.Close()
	inode, err := CurrentNetNSInode()
	if err != nil {
		return err
	}
	if err := store.RequireEmptyNamespace(inode); err != nil {
		return err
	}
	sub, err := SubscribePM()
	if err != nil {
		return fmt.Errorf("subscribe MPTCP PM before readiness: %w", err)
	}
	defer sub.Close()
	var listener net.Listener
	if path, ok := strings.CutPrefix(listen, "unix:"); ok {
		if !strings.HasPrefix(path, "/") {
			return errors.New("Unix listen path must be absolute")
		}
		listener, err = net.Listen("unix", path)
		if err == nil {
			if err = os.Chmod(path, 0600); err != nil {
				listener.Close()
				return err
			}
		}
	} else {
		addr, parseErr := netip.ParseAddrPort(listen)
		if parseErr != nil || !addr.Addr().IsLoopback() {
			return errors.New("TCP listen must be a literal loopback IP:port")
		}
		listener, err = net.Listen("tcp", listen)
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	d := NewDaemon(store, inode, ValidateLegAddress)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() {
		err := sub.Run(runCtx, d.Observe)
		if err != nil && !errors.Is(err, context.Canceled) {
			d.Fail(err)
		}
		failures <- err
	})
	workers.Go(func() { failures <- server.Serve(listener) })
	_, _ = fmt.Fprintf(out, "dualsteer-agent ready listen=%s netns=%d; PM subscribed; fresh connections only\n", listen, inode)
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()
	var cause error
	running := true
	for running {
		select {
		case <-ctx.Done():
			running = false
		case cause = <-failures:
			running = false
		case <-retry.C:
			_ = d.Retry()
		}
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	workers.Wait()
	cleanupErr := d.Cleanup()
	if errors.Is(cause, http.ErrServerClosed) || errors.Is(cause, context.Canceled) {
		cause = nil
	}
	return errors.Join(cause, shutdownErr, cleanupErr)
}
