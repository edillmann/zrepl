package pruner

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"github.com/zrepl/zrepl/config"
	"github.com/zrepl/zrepl/logger"
	"github.com/zrepl/zrepl/pruning"
	"github.com/zrepl/zrepl/replication/pdu"
	"net"
	"sync"
	"time"
)

// Try to keep it compatible with gitub.com/zrepl/zrepl/replication.Endpoint
type History interface {
	SnapshotReplicationStatus(ctx context.Context, req *pdu.SnapshotReplicationStatusReq) (*pdu.SnapshotReplicationStatusRes, error)
}

type Target interface {
	ListFilesystems(ctx context.Context) ([]*pdu.Filesystem, error)
	ListFilesystemVersions(ctx context.Context, fs string) ([]*pdu.FilesystemVersion, error) // fix depS
	DestroySnapshots(ctx context.Context, req *pdu.DestroySnapshotsReq) (*pdu.DestroySnapshotsRes, error)
}

type Logger = logger.Logger

type contextKey int

const contextKeyLogger contextKey = 0

func WithLogger(ctx context.Context, log Logger) context.Context {
	return context.WithValue(ctx, contextKeyLogger, log)
}

func GetLogger(ctx context.Context) Logger {
	if l, ok := ctx.Value(contextKeyLogger).(Logger); ok {
		return l
	}
	return logger.NewNullLogger()
}

type args struct {
	ctx       context.Context
	target    Target
	receiver  History
	rules     []pruning.KeepRule
	retryWait time.Duration
}

type Pruner struct {
	args args

	mtx sync.RWMutex

	state State

	// State ErrWait|ErrPerm
	sleepUntil time.Time
	err        error

	// State Exec
	prunePending   []fs
	pruneCompleted []fs
}

type PrunerFactory struct {
	senderRules   []pruning.KeepRule
	receiverRules []pruning.KeepRule
	retryWait     time.Duration
}

func checkContainsKeep1(rules []pruning.KeepRule) error {
	if len(rules) == 0 {
		return nil //No keep rules means keep all - ok
	}
	for _, e := range rules {
		switch e.(type) {
		case *pruning.KeepLastN:
			return nil
		}
	}
	return errors.New("sender keep rules must contain last_n or be empty so that the last snapshot is definitely kept")
}

func NewPrunerFactory(in config.PruningSenderReceiver) (*PrunerFactory, error) {
	keepRulesReceiver, err := pruning.RulesFromConfig(in.KeepReceiver)
	if err != nil {
		return nil, errors.Wrap(err, "cannot build receiver pruning rules")
	}

	keepRulesSender, err := pruning.RulesFromConfig(in.KeepSender)
	if err != nil {
		return nil, errors.Wrap(err, "cannot build sender pruning rules")
	}

	if err := checkContainsKeep1(keepRulesSender); err != nil {
		return nil, err
	}

	f := &PrunerFactory{
		keepRulesSender,
		keepRulesReceiver,
		10 * time.Second, //FIXME constant
	}
	return f, nil
}

func (f *PrunerFactory) BuildSenderPruner(ctx context.Context, target Target, receiver History) *Pruner {
	p := &Pruner{
		args: args{
			WithLogger(ctx, GetLogger(ctx).WithField("prune_side", "sender")),
			target,
			receiver,
			f.senderRules,
			f.retryWait,
		},
		state: Plan,
	}
	return p
}

func (f *PrunerFactory) BuildReceiverPruner(ctx context.Context, target Target, receiver History) *Pruner {
	p := &Pruner{
		args: args{
			WithLogger(ctx, GetLogger(ctx).WithField("prune_side", "receiver")),
			target,
			receiver,
			f.receiverRules,
			f.retryWait,
		},
		state: Plan,
	}
	return p
}

//go:generate stringer -type=State
type State int

const (
	Plan State = 1 << iota
	PlanWait
	Exec
	ExecWait
	ErrPerm
	Done
)

func (s State) statefunc() state {
	var statemap = map[State]state{
		Plan:     statePlan,
		PlanWait: statePlanWait,
		Exec:     stateExec,
		ExecWait: stateExecWait,
		ErrPerm:  nil,
		Done:     nil,
	}
	return statemap[s]
}

type updater func(func(*Pruner)) State
type state func(args *args, u updater) state

func (p *Pruner) Prune() {
	p.prune(p.args)
}

func (p *Pruner) prune(args args) {
	s := p.state.statefunc()
	for s != nil {
		pre := p.state
		s = s(&args, func(f func(*Pruner)) State {
			p.mtx.Lock()
			defer p.mtx.Unlock()
			f(p)
			return p.state
		})
		post := p.state
		GetLogger(args.ctx).
			WithField("transition", fmt.Sprintf("%s=>%s", pre, post)).
			Debug("state transition")
	}
}

func (p *Pruner) Report() interface{} {
	return nil // FIXME TODO
}

type fs struct {
	path  string
	snaps []pruning.Snapshot

	mtx sync.RWMutex
	// for Plan
	err error
}

func (f *fs) Update(err error) {
	f.mtx.Lock()
	defer f.mtx.Unlock()
	f.err = err
}

type snapshot struct {
	replicated bool
	date       time.Time
	fsv        *pdu.FilesystemVersion
}

var _ pruning.Snapshot = snapshot{}

func (s snapshot) Name() string { return s.fsv.Name }

func (s snapshot) Replicated() bool { return s.replicated }

func (s snapshot) Date() time.Time { return s.date }

func shouldRetry(e error) bool {
	switch e.(type) {
	case nil:
		return true
	case net.Error:
		return true
	}
	return false
}

func onErr(u updater, e error) state {
	return u(func(p *Pruner) {
		p.err = e
		if !shouldRetry(e) {
			p.state = ErrPerm
			return
		}
		switch p.state {
		case Plan:
			p.state = PlanWait
		case Exec:
			p.state = ExecWait
		default:
			panic(p.state)
		}
	}).statefunc()
}

func statePlan(a *args, u updater) state {

	ctx, target, receiver := a.ctx, a.target, a.receiver

	tfss, err := target.ListFilesystems(ctx)
	if err != nil {
		return onErr(u, err)
	}

	pfss := make([]fs, len(tfss))
	for i, tfs := range tfss {
		tfsvs, err := target.ListFilesystemVersions(ctx, tfs.Path)
		if err != nil {
			return onErr(u, err)
		}

		pfs := fs{
			path:  tfs.Path,
			snaps: make([]pruning.Snapshot, 0, len(tfsvs)),
		}

		for _, tfsv := range tfsvs {
			if tfsv.Type != pdu.FilesystemVersion_Snapshot {
				continue
			}
			creation, err := tfsv.CreationAsTime()
			if err != nil {
				return onErr(u, fmt.Errorf("%s%s has invalid creation date: %s", tfs, tfsv.RelName(), err))
			}
			req := pdu.SnapshotReplicationStatusReq{
				Filesystem: tfs.Path,
				Snapshot:   tfsv.Name,
				Op:         pdu.SnapshotReplicationStatusReq_Get,
			}
			res, err := receiver.SnapshotReplicationStatus(ctx, &req)
			if err != nil {
				GetLogger(ctx).
					WithField("req", req.String()).
					WithError(err).Error("cannot get snapshot replication status")
			}
			if err != nil && shouldRetry(err) {
				return onErr(u, err)
			} else if err != nil {
				pfs.err = err
				pfs.snaps = nil
				break
			}
			if res.Status == pdu.SnapshotReplicationStatusRes_Nonexistent {
				GetLogger(ctx).
					Debug("snapshot does not exist in history, assuming was replicated")
			}
			pfs.snaps = append(pfs.snaps, snapshot{
				replicated: !(res.Status != pdu.SnapshotReplicationStatusRes_Replicated),
				date:       creation,
				fsv:        tfsv,
			})

		}

		pfss[i] = pfs

	}

	return u(func(pruner *Pruner) {
		for _, pfs := range pfss {
			if pfs.err != nil {
				pruner.pruneCompleted = append(pruner.pruneCompleted, pfs)
			} else {
				pruner.prunePending = append(pruner.prunePending, pfs)
			}
		}
		pruner.state = Exec
	}).statefunc()
}

func stateExec(a *args, u updater) state {

	var pfs fs
	state := u(func(pruner *Pruner) {
		if len(pruner.prunePending) == 0 {
			pruner.state = Done
			return
		}
		pfs = pruner.prunePending[0]
	})
	if state != Exec {
		return state.statefunc()
	}

	GetLogger(a.ctx).Debug(fmt.Sprintf("%#v", a.rules))
	destroyListI := pruning.PruneSnapshots(pfs.snaps, a.rules)
	destroyList := make([]*pdu.FilesystemVersion, len(destroyListI))
	for i := range destroyList {
		destroyList[i] = destroyListI[i].(snapshot).fsv
		GetLogger(a.ctx).
			WithField("fs", pfs.path).
			WithField("destroy_snap", destroyList[i].Name).
			Debug("policy destroys snapshot")
	}
	pfs.Update(nil)
	req := pdu.DestroySnapshotsReq{
		Filesystem: pfs.path,
		Snapshots:  destroyList,
	}
	_, err := a.target.DestroySnapshots(a.ctx, &req)
	pfs.Update(err)
	if err != nil && shouldRetry(err) {
		return onErr(u, err)
	}
	// if it's not retryable, treat is like as being done

	return u(func(pruner *Pruner) {
		pruner.pruneCompleted = append(pruner.pruneCompleted, pfs)
		pruner.prunePending = pruner.prunePending[1:]
	}).statefunc()
}

func stateExecWait(a *args, u updater) state {
	return doWait(Exec, a, u)
}

func statePlanWait(a *args, u updater) state {
	return doWait(Plan, a, u)
}

func doWait(goback State, a *args, u updater) state {
	timer := time.NewTimer(a.retryWait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return u(func(pruner *Pruner) {
			pruner.state = goback
		}).statefunc()
	case <-a.ctx.Done():
		return onErr(u, a.ctx.Err())
	}
}