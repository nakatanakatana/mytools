package sshgw

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"charm.land/ssh"
	"charm.land/wish/v2"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/config"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/service"
	gossh "golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/remotecommand"
)

// GatewayService is the subset of service.Service needed by the SSH gateway.
type GatewayService interface {
	Config() config.Config
	Overview(ctx context.Context) (*service.DashboardData, error)
	Create(ctx context.Context, req service.CreateRequest) (*service.CreateResult, error)
	Suspend(ctx context.Context, name string) error
	Resume(ctx context.Context, name string) error
	EnsureRunningAndReady(ctx context.Context, name string) (*k8s.SandboxInfo, error)
	CreateCheckpoint(ctx context.Context, sandboxName, checkpointName string) (string, error)
	DeleteCheckpoint(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	Exec(ctx context.Context, sandboxName string, opts k8s.ExecOptions) error
}

// Server wraps a charmbracelet/wish SSH server for sandbox management and direct shell attach.
type Server struct {
	wishServer *ssh.Server
	keyStore   *KeyStore
	svc        GatewayService
	logger     *slog.Logger
}

var managementCommands = []string{"ls", "list", "new", "create", "checkpoint", "cp", "suspend", "resume", "rm", "delete", "help"}
var managementUsers = []string{"admin", "ctl", "sandbox-ctl", "root"}

// NewServer builds the embedded SSH gateway server.
func NewServer(svc GatewayService, keyStore *KeyStore, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg := svc.Config()
	s := &Server{
		keyStore: keyStore,
		svc:      svc,
		logger:   logger,
	}

	opts := []ssh.Option{
		wish.WithAddress(cfg.SSHAddr),
		wish.WithPublicKeyAuth(func(_ ssh.Context, key ssh.PublicKey) bool {
			return keyStore.Authorize(key)
		}),
		wish.WithMiddleware(s.routeMiddleware),
	}

	if strings.TrimSpace(cfg.SSHHostKeyPath) != "" {
		opts = append(opts, wish.WithHostKeyPath(cfg.SSHHostKeyPath))
	} else {
		pemBytes, err := generateInMemoryHostKeyPEM()
		if err != nil {
			return nil, fmt.Errorf("generate in-memory ssh host key: %w", err)
		}
		opts = append(opts, wish.WithHostKeyPEM(pemBytes))
	}

	ws, err := wish.NewServer(opts...)
	if err != nil {
		return nil, fmt.Errorf("create wish ssh server: %w", err)
	}
	s.wishServer = ws
	return s, nil
}

// ListenAndServe starts accepting SSH connections.
func (s *Server) ListenAndServe() error {
	return s.wishServer.ListenAndServe()
}

// Shutdown gracefully stops the SSH server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.wishServer.Shutdown(ctx)
}

func (s *Server) routeMiddleware(next ssh.Handler) ssh.Handler {
	return func(sess ssh.Session) {
		cmd := sess.Command()
		user := sess.User()

		if isManagementSession(user, cmd) {
			code := s.runManagementCommand(sess.Context(), cmd, sess)
			_ = sess.Exit(code)
			return
		}

		// Direct shell attach: ssh <sandbox>@sandbox-ctl [optional command...]
		code := s.runDirectExec(sess, user, cmd)
		_ = sess.Exit(code)
		_ = next
	}
}

func isManagementSession(user string, cmd []string) bool {
	if len(cmd) > 0 && slices.Contains(managementCommands, cmd[0]) {
		return true
	}
	return slices.Contains(managementUsers, user)
}

func (s *Server) runManagementCommand(ctx context.Context, args []string, out io.Writer) int {
	if len(args) == 0 {
		printHelp(out)
		return 0
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "help":
		printHelp(out)
		return 0
	case "ls", "list":
		return s.cmdList(ctx, out)
	case "new", "create":
		return s.cmdNew(ctx, rest, out)
	case "checkpoint", "cp":
		return s.cmdCheckpoint(ctx, rest, out)
	case "suspend":
		return s.cmdSuspend(ctx, rest, out)
	case "resume":
		return s.cmdResume(ctx, rest, out)
	case "rm", "delete":
		return s.cmdRemove(ctx, rest, out)
	default:
		_, _ = fmt.Fprintf(out, "unknown command %q\n\n", sub)
		printHelp(out)
		return 1
	}
}

func (s *Server) cmdList(ctx context.Context, out io.Writer) int {
	data, err := s.svc.Overview(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WARMPOOLS")
	_, _ = fmt.Fprintln(tw, "NAME\tTEMPLATE\tREADY/DESIRED")
	for _, wp := range data.WarmPools {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d/%d\n", wp.Name, wp.TemplateName, wp.ReadyReplicas, wp.DesiredReplicas)
	}
	_, _ = fmt.Fprintln(tw)
	_, _ = fmt.Fprintln(tw, "SANDBOXES")
	_, _ = fmt.Fprintln(tw, "NAME\tPHASE\tTEMPLATE\tCLAIM\tPERSISTENT")
	for _, sb := range data.Sandboxes {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\n", sb.Name, sb.Phase, sb.TemplateName, sb.ClaimName, sb.Persistent)
	}
	_, _ = fmt.Fprintln(tw)
	_, _ = fmt.Fprintln(tw, "CHECKPOINTS")
	_, _ = fmt.Fprintln(tw, "NAME\tSOURCE_PVC\tREADY\tSIZE")
	for _, cp := range data.Checkpoints {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\t%s\n", cp.Name, cp.SourcePVC, cp.ReadyToUse, cp.RestoreSize)
	}
	_ = tw.Flush()
	return 0
}

func (s *Server) cmdNew(ctx context.Context, args []string, out io.Writer) int {
	cfg := s.svc.Config()
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(out)

	tmpl := fs.String("template", cfg.EphemeralTemplate, "sandbox template (opencode-ephemeral or opencode-persistent)")
	ttlStr := fs.String("ttl", "", "TTL duration (e.g. 2h)")
	fromCP := fs.String("from", "", "checkpoint VolumeSnapshot name to fork from")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := ""
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	var ttl time.Duration
	if *ttlStr != "" {
		d, err := time.ParseDuration(*ttlStr)
		if err != nil {
			_, _ = fmt.Fprintf(out, "error: invalid --ttl %q: %v\n", *ttlStr, err)
			return 2
		}
		ttl = d
	}

	res, err := s.svc.Create(ctx, service.CreateRequest{
		Name:       name,
		Template:   *tmpl,
		TTL:        ttl,
		Checkpoint: *fromCP,
	})
	if err != nil {
		_, _ = fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "created %s %s (template=%s)\n", res.Kind, res.Name, res.Template)
	return 0
}

func (s *Server) cmdCheckpoint(ctx context.Context, args []string, out io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(out, "usage: checkpoint <sandbox> [checkpoint-name] | checkpoint rm <name>")
		return 2
	}
	if args[0] == "rm" || args[0] == "delete" {
		if len(args) < 2 {
			_, _ = fmt.Fprintln(out, "usage: checkpoint rm <name>")
			return 2
		}
		if err := s.svc.DeleteCheckpoint(ctx, args[1]); err != nil {
			_, _ = fmt.Fprintf(out, "error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "deleted checkpoint %s\n", args[1])
		return 0
	}
	cpName := ""
	if len(args) > 1 {
		cpName = args[1]
	}
	created, err := s.svc.CreateCheckpoint(ctx, args[0], cpName)
	if err != nil {
		_, _ = fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "created checkpoint %s\n", created)
	return 0
}

func (s *Server) cmdSuspend(ctx context.Context, args []string, out io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprintln(out, "usage: suspend <sandbox>")
		return 2
	}
	if err := s.svc.Suspend(ctx, args[0]); err != nil {
		_, _ = fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "suspended %s\n", args[0])
	return 0
}

func (s *Server) cmdResume(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(out)
	wait := fs.Bool("wait", false, "wait until sandbox reports Ready")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		_, _ = fmt.Fprintln(out, "usage: resume [--wait] <sandbox>")
		return 2
	}
	name := fs.Arg(0)
	if *wait {
		if _, err := s.svc.EnsureRunningAndReady(ctx, name); err != nil {
			_, _ = fmt.Fprintf(out, "error: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "resumed and ready: %s\n", name)
		return 0
	}
	if err := s.svc.Resume(ctx, name); err != nil {
		_, _ = fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "resuming %s\n", name)
	return 0
}

func (s *Server) cmdRemove(ctx context.Context, args []string, out io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprintln(out, "usage: rm <sandbox>")
		return 2
	}
	if err := s.svc.Delete(ctx, args[0]); err != nil {
		_, _ = fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "deleted %s\n", args[0])
	return 0
}

func (s *Server) runDirectExec(sess ssh.Session, sandboxName string, cmd []string) int {
	ptyReq, winCh, isPty := sess.Pty()
	var sizeQueue remotecommand.TerminalSizeQueue
	if isPty {
		q := newTerminalSizeQueue(ptyReq.Window, winCh, sess.Context())
		sizeQueue = q
	}

	err := s.svc.Exec(sess.Context(), sandboxName, k8s.ExecOptions{
		Command:      cmd,
		TTY:          isPty,
		Stdin:        sess,
		Stdout:       sess,
		Stderr:       sess.Stderr(),
		TerminalSize: sizeQueue,
	})
	if err != nil {
		_, _ = fmt.Fprintf(sess.Stderr(), "sandbox exec error: %v\n", err)
		return 1
	}
	return 0
}

func printHelp(out io.Writer) {
	_, _ = fmt.Fprintln(out, `sandbox-control SSH gateway

Management commands (ssh sandbox-ctl <command>):
  ls                                                  List warm pools, sandboxes, and checkpoints
  new [name] [--template=T] [--ttl=2h] [--from=CP]    Create a sandbox or fork from a checkpoint
  checkpoint <sandbox> [name]                         Create a VolumeSnapshot checkpoint
  checkpoint rm <name>                                Delete a VolumeSnapshot checkpoint
  suspend <sandbox>                                   Suspend a sandbox
  resume [--wait] <sandbox>                           Resume a sandbox
  rm <sandbox>                                        Delete a sandbox or claim

Direct shell:
  ssh <sandbox-name>@sandbox-ctl                      Auto-resume if suspended and attach TTY`)
}

type terminalSizeQueue struct {
	ch chan remotecommand.TerminalSize
}

func newTerminalSizeQueue(initial ssh.Window, winCh <-chan ssh.Window, ctx context.Context) *terminalSizeQueue {
	q := &terminalSizeQueue{ch: make(chan remotecommand.TerminalSize, 8)}
	if initial.Width > 0 && initial.Height > 0 {
		q.ch <- remotecommand.TerminalSize{
			Width:  uint16(initial.Width),
			Height: uint16(initial.Height),
		}
	}
	go func() {
		defer close(q.ch)
		for {
			select {
			case <-ctx.Done():
				return
			case win, ok := <-winCh:
				if !ok {
					return
				}
				if win.Width <= 0 || win.Height <= 0 {
					continue
				}
				select {
				case q.ch <- remotecommand.TerminalSize{Width: uint16(win.Width), Height: uint16(win.Height)}:
				default:
				}
			}
		}
	}()
	return q
}

func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	sz, ok := <-q.ch
	if !ok {
		return nil
	}
	return &sz
}

func generateInMemoryHostKeyPEM() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}
