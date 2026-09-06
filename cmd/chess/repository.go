package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	application "github.com/generalbusiness-ai/gitseq-chess"
	"github.com/generalbusiness-ai/gitseq/host"
	"github.com/generalbusiness-ai/gitseq/host/identity"
)

// Repository selection and configuration come from explicit arguments and the
// operator's on-disk Git configuration, never ambient GIT_DIR or injected -c
// equivalents. SSH agents and ordinary transport environment remain available.
func chessGitCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// The pinned host inherits its process environment when it opens Git stores.
// Refuse overrides before entering either host or transport code: changing the
// global environment around a host call would race concurrent HTTP/MCP work.
func requireExplicitGitEnvironment() error {
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "GIT_") {
			return errors.New("unset inherited GIT_* variables before running Chess; repository and configuration must be explicit")
		}
	}
	return nil
}

var errDeliveryPending = errors.New("delivery is pending; no new durable action is accepted")

// The local Git configuration is shared by every linked worktree and writer.
// It contains a key path, never a key. Configure all four values explicitly;
// an incomplete forge configuration fails closed, including for native CLI.
type forgeConfig struct{ remote, ref, genesis, key string }

func loadForgeConfig(ctx context.Context, repo string) (*forgeConfig, error) {
	if err := requireExplicitGitEnvironment(); err != nil {
		return nil, err
	}
	values := make([]string, 4)
	for i, name := range []string{"chess.forgeRemote", "chess.forgeRef", "chess.forgeGenesis", "chess.sequencerKey"} {
		out, err := chessGitCommand(ctx, "-C", repo, "config", "--local", "--get-all", name).Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		value := strings.TrimSuffix(string(out), "\n")
		if strings.ContainsAny(value, "\r\n\x00") || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("%s must have one exact value", name)
		}
		values[i] = value
	}
	if strings.Join(values, "") == "" {
		return nil, nil
	}
	for _, value := range values {
		if value == "" {
			return nil, errors.New("forge configuration requires remote, ref, genesis and sequencer key path")
		}
	}
	c := &forgeConfig{values[0], values[1], values[2], values[3]}
	if len(c.genesis) != 40 && len(c.genesis) != 64 {
		return nil, errors.New("forge genesis must be a full object ID")
	}
	for _, ch := range c.genesis {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return nil, errors.New("forge genesis must be canonical lowercase hex")
		}
	}
	if c.ref != "refs/seq/"+c.genesis {
		return nil, errors.New("forge ref must be the exact refs/seq/<genesis> ref")
	}
	if strings.HasPrefix(c.remote, "-") || strings.ContainsAny(c.remote, " /:\\") {
		return nil, errors.New("forge remote must be a configured Git remote name")
	}
	var destination string
	for _, direction := range [][]string{{"--all"}, {"--push", "--all"}} {
		args := append([]string{"-C", repo, "remote", "get-url"}, direction...)
		out, err := chessGitCommand(ctx, append(args, c.remote)...).Output()
		url := strings.TrimSuffix(string(out), "\n")
		if err != nil || url == "" || strings.ContainsAny(url, "\r\n") {
			return nil, errors.New("forge requires one configured fetch and push destination")
		}
		if destination != "" && destination != url {
			return nil, errors.New("forge fetch and push destinations must match")
		}
		destination = url
	}
	return c, nil
}

// chessRepository serializes local append and forge confirmation. Read views
// hold an immutable verified prefix; no handler reads the pending local head.
type chessRepository struct {
	repo      string
	owner     *writerOwnership
	workspace *host.Workspace
	forge     *forgeConfig
	write     sync.Mutex
	mu        sync.RWMutex
	confirmed host.Log
	stopped   error
	git       func(context.Context, ...string) (string, error)
}

func openChessRepository(ctx context.Context, repo string) (_ *chessRepository, resultErr error) {
	if err := requireExplicitGitEnvironment(); err != nil {
		return nil, err
	}
	owner, err := acquireWriter(ctx, repo)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			owner.Close()
		}
	}()
	cfg, err := loadForgeConfig(ctx, repo)
	if err != nil {
		return nil, err
	}
	s := &chessRepository{repo: repo, owner: owner, forge: cfg}
	s.git = func(ctx context.Context, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := chessGitCommand(ctx, append([]string{"-C", repo}, args...)...)
		out, err := cmd.Output()
		// Do not expose Git stderr: transport diagnostics can include credential URLs.
		if err != nil {
			return "", fmt.Errorf("forge Git %s failed: %w", args[0], err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if cfg == nil {
		s.workspace, err = host.Open(ctx, repo, application.Application)
		if err == nil {
			s.confirmed, err = s.workspace.Records(ctx)
		}
		return s, err
	}
	retained, retainedErr := s.git(ctx, "rev-parse", "--verify", "--quiet", "refs/chess/forge-confirmed/"+cfg.genesis)
	if retainedErr != nil {
		var exit *exec.ExitError
		if !errors.As(retainedErr, &exit) || exit.ExitCode() != 1 {
			return nil, retainedErr
		}
	}
	remote, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	if retained != "" {
		ok, err := s.ancestor(ctx, retained, remote)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("forge rolled back or diverged from retained confirmation; preserve history and reconcile explicitly")
		}
	}
	local, localErr := s.git(ctx, "rev-parse", "--verify", cfg.ref)
	if localErr != nil {
		if _, err = s.git(ctx, "update-ref", cfg.ref, remote, strings.Repeat("0", len(cfg.genesis))); err != nil {
			return nil, err
		}
	} else if local != remote {
		ahead, err := s.ancestor(ctx, local, remote)
		if err != nil {
			return nil, err
		}
		if ahead {
			if _, err = s.git(ctx, "update-ref", cfg.ref, remote, local); err != nil {
				return nil, err
			}
		} else {
			pending, err := s.ancestor(ctx, remote, local)
			if err != nil {
				return nil, err
			}
			if !pending {
				return nil, errors.New("local and forge histories diverge; preserve both and reconcile explicitly")
			}
		}
	}
	// OpenAttached verifies the complete signed sequence and its exact binding.
	// The host does not consume sequencer custody until a subsequent append.
	s.workspace, err = host.OpenAttached(ctx, repo, application.Application, host.Attachment{Genesis: cfg.genesis, SequencerKey: cfg.key})
	if err != nil {
		return nil, err
	}
	log, err := s.workspace.Records(ctx)
	if err != nil {
		return nil, err
	}
	s.confirmed, err = verifiedPrefix(log, remote)
	if err != nil {
		return nil, err
	}
	// A pending binding replacement cannot make an incompatible remote binding
	// appear current. Rebinding in forge mode is an explicit operator operation.
	if len(log.Records) > 0 {
		for _, r := range log.Records[len(s.confirmed.Records):] {
			if r.Schema == log.Records[0].Schema {
				return nil, errors.New("pending binding change requires operator reconciliation")
			}
		}
	}
	if _, err = s.git(ctx, "update-ref", "refs/chess/forge-confirmed/"+cfg.genesis, remote); err != nil {
		return nil, err
	}
	if log.Head != remote {
		if err := s.confirm(ctx); err != nil && !errors.Is(err, errDeliveryPending) {
			return nil, err
		}
	}
	return s, nil
}

func (s *chessRepository) Close() error { return s.owner.Close() }

func verifiedPrefix(log host.Log, head string) (host.Log, error) {
	n := -1
	if head == log.Genesis {
		n = 0
	}
	for i, r := range log.Records {
		if strings.HasSuffix(r.ID, ":"+head) {
			n = i + 1
			break
		}
	}
	if n < 0 {
		return host.Log{}, errors.New("forge head is absent from the verified local sequence")
	}
	// Host records are immutable borrowed values. Own the slice header/backing
	// array so later host suffix appends cannot alter this published prefix.
	return host.Log{Genesis: log.Genesis, Head: head, Depth: log.Depth - len(log.Records) + n, Records: append([]host.Record(nil), log.Records[:n]...)}, nil
}

func (s *chessRepository) fetch(ctx context.Context) (string, error) {
	c := s.forge
	tracking := "refs/chess/forge-observed/" + c.genesis
	// Fetch may replace only this observation ref. The sequence and confirmed
	// refs remain untouched until verification and ancestry checks succeed.
	if _, err := s.git(ctx, "fetch", "--no-tags", "--no-write-fetch-head", c.remote, "+"+c.ref+":"+tracking); err != nil {
		return "", err
	}
	return s.git(ctx, "rev-parse", "--verify", tracking)
}
func (s *chessRepository) ancestor(ctx context.Context, a, b string) (bool, error) {
	_, err := s.git(ctx, "merge-base", "--is-ancestor", a, b)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

func (s *chessRepository) view(ctx context.Context) (*repositoryView, error) {
	if s.forge == nil {
		log, err := s.workspace.Records(ctx)
		if err != nil {
			return nil, err
		}
		return &repositoryView{repository: s, log: log}, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped != nil {
		return nil, s.stopped
	}
	return &repositoryView{repository: s, log: s.confirmed}, nil
}

func (s *chessRepository) ready(ctx context.Context) error {
	if err := s.owner.checkFile(); err != nil {
		return err
	}
	s.mu.RLock()
	stopped := s.stopped
	confirmed := s.confirmed.Head
	s.mu.RUnlock()
	if stopped != nil {
		return stopped
	}
	if s.forge != nil {
		log, err := s.workspace.Records(ctx)
		if err != nil {
			return err
		}
		if log.Head != confirmed {
			return errDeliveryPending
		}
	}
	return nil
}

// confirm is called with write ownership. Each attempt first checks remote
// containment, including after a lost push response, then pushes the same
// immutable head without force. At most three attempts; no unbounded retry.
func (s *chessRepository) confirm(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := s.owner.checkFile(); err != nil {
		return err
	}
	log, err := s.workspace.Records(ctx)
	if err != nil {
		return err
	}
	if s.forge == nil {
		return nil
	}
	s.mu.RLock()
	previous := s.confirmed.Head
	s.mu.RUnlock()
	for attempt := 0; attempt < 3; attempt++ {
		remote, fetchErr := s.fetch(ctx)
		if fetchErr == nil {
			forward, err := s.ancestor(ctx, previous, remote)
			contained, containErr := s.ancestor(ctx, remote, log.Head)
			if err == nil && containErr == nil {
				if !forward || !contained {
					err := errors.New("forge history changed outside the owned writer; preserve pending history and reconcile explicitly")
					s.mu.Lock()
					s.stopped = err
					s.mu.Unlock()
					return err
				}
				if remote != log.Head {
					_, _ = s.git(ctx, "push", "--porcelain", s.forge.remote, log.Head+":"+s.forge.ref)
					// A lost push response can still mean success. Observe the exact
					// remote ref before publishing or acknowledging this prefix.
					remote, fetchErr = s.fetch(ctx)
				}
				if fetchErr == nil && remote == log.Head {
					prefix, err := verifiedPrefix(log, remote)
					if err != nil {
						return err
					}
					if _, err = s.git(ctx, "update-ref", "refs/chess/forge-confirmed/"+s.forge.genesis, remote); err != nil {
						return errDeliveryPending
					}
					s.mu.Lock()
					s.confirmed = prefix
					s.mu.Unlock()
					return nil
				}
			}
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errDeliveryPending
			case <-timer.C:
			}
		}
	}
	return errDeliveryPending
}

func (s *chessRepository) appendSigned(ctx context.Context, act host.SignedAct) (host.Record, error) {
	if !s.write.TryLock() {
		return host.Record{}, errDeliveryPending
	}
	defer s.write.Unlock()
	// Internal handler constructors used without a process owner still obey
	// the shared lock. The supported serve entry point holds it for its lifetime.
	if s.owner == nil {
		owner, err := acquireWriter(ctx, s.repo)
		if err != nil {
			return host.Record{}, err
		}
		s.owner = owner
		defer func() { owner.Close(); s.owner = nil }()
	}
	if err := s.ready(ctx); err != nil {
		if errors.Is(err, errDeliveryPending) {
			_ = s.confirm(ctx)
		}
		return host.Record{}, err
	}
	record, err := s.workspace.AppendSigned(ctx, act)
	confirmation := s.confirm(ctx)
	if confirmation != nil {
		return record, confirmation
	}
	return record, err
}

type repositoryView struct {
	repository *chessRepository
	log        host.Log
}

func (v *repositoryView) Records(context.Context) (host.Log, error) { return v.log, nil }
func (v *repositoryView) Prepare(act host.Act) (host.PreparedAct, error) {
	s := v.repository
	s.write.Lock()
	defer s.write.Unlock()
	if s.owner != nil {
		if err := s.ready(context.Background()); err != nil {
			return host.PreparedAct{}, err
		}
	}
	return s.workspace.Prepare(act)
}

// prepareReplay only reconstructs bytes for signature verification. Unlike a
// new preparation, it must work while an earlier append awaits forge delivery:
// AppendSigned will reconcile that exact pending history before any intake.
func (v *repositoryView) prepareReplay(act host.Act) (host.PreparedAct, error) {
	s := v.repository
	if !s.write.TryLock() {
		return host.PreparedAct{}, errors.New("repository is busy")
	}
	defer s.write.Unlock()
	if s.owner != nil {
		if err := s.owner.checkFile(); err != nil {
			return host.PreparedAct{}, err
		}
	}
	return s.workspace.Prepare(act)
}

func (v *repositoryView) PrepareEndorsement(ctx context.Context, a identity.Anchor, key string) (host.PreparedAct, error) {
	s := v.repository
	s.write.Lock()
	defer s.write.Unlock()
	if s.owner != nil {
		if err := s.ready(ctx); err != nil {
			return host.PreparedAct{}, err
		}
	}
	return identity.PrepareEndorsement(ctx, s.workspace, a, key)
}
func (v *repositoryView) AppendSigned(ctx context.Context, a host.SignedAct) (host.Record, error) {
	r, err := v.repository.appendSigned(ctx, a)
	if err == nil {
		fresh, e := v.repository.view(ctx)
		if e != nil {
			return r, e
		}
		v.log = fresh.log
	}
	return r, err
}

type repositoryOpener func(context.Context) (*repositoryView, application.Projection, error)

func (s *chessRepository) openView(ctx context.Context) (*repositoryView, application.Projection, error) {
	v, err := s.view(ctx)
	if err != nil {
		return nil, application.Projection{}, err
	}
	return v, application.Fold(v.log), nil
}
func localRepositoryOpener(repo string) repositoryOpener {
	return func(ctx context.Context) (*repositoryView, application.Projection, error) {
		cfg, err := loadForgeConfig(ctx, repo)
		if err != nil {
			return nil, application.Projection{}, err
		}
		if cfg != nil {
			return nil, application.Projection{}, errors.New("forge reads require the owned confirmed repository")
		}
		ws, err := host.Open(ctx, repo, application.Application)
		if err != nil {
			return nil, application.Projection{}, err
		}
		return (&chessRepository{repo: repo, workspace: ws}).openView(ctx)
	}
}

type confirmedLog struct{ log host.Log }

func (r confirmedLog) Records(context.Context) (host.Log, error) { return r.log, nil }

// Read-only CLI and MCP operations use the last prefix published by the owned
// service. They need no writer lease and cannot reveal a local pending suffix.
func readConfirmed(ctx context.Context, repo string) (application.RecordReader, application.Projection, error) {
	cfg, err := loadForgeConfig(ctx, repo)
	if err != nil {
		return nil, application.Projection{}, err
	}
	if cfg == nil {
		return application.OpenProjection(ctx, repo)
	}
	ws, err := host.OpenAttached(ctx, repo, application.Application, host.Attachment{Genesis: cfg.genesis, SequencerKey: cfg.key})
	if err != nil {
		return nil, application.Projection{}, err
	}
	out, err := chessGitCommand(ctx, "-C", repo, "rev-parse", "--verify", "refs/chess/forge-confirmed/"+cfg.genesis).Output()
	if err != nil {
		return nil, application.Projection{}, errors.New("no forge-confirmed frontier is available")
	}
	log, err := ws.Records(ctx)
	if err != nil {
		return nil, application.Projection{}, err
	}
	log, err = verifiedPrefix(log, strings.TrimSpace(string(out)))
	if err != nil {
		return nil, application.Projection{}, err
	}
	return confirmedLog{log}, application.Fold(log), nil
}
