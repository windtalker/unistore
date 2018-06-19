package tikv

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pingcap/tidb/kv"
	"golang.org/x/net/context"
)

var _ tikvpb.TikvServer = new(Server)

type Server struct {
	mvccStore     *MVCCStore
	regionManager *RegionManager
	wg            sync.WaitGroup
	refCount      int32
	stopped       int32
}

func NewServer(rm *RegionManager, store *MVCCStore) *Server {
	return &Server{
		mvccStore:     store,
		regionManager: rm,
	}
}

const requestMaxSize = 6 * 1024 * 1024

func (svr *Server) checkRequestSize(size int) *errorpb.Error {
	// TiKV has a limitation on raft log size.
	// mocktikv has no raft inside, so we check the request's size instead.
	if size >= requestMaxSize {
		return &errorpb.Error{
			RaftEntryTooLarge: &errorpb.RaftEntryTooLarge{},
		}
	}
	return nil
}

func (svr *Server) Stop() {
	atomic.StoreInt32(&svr.stopped, 1)
	for {
		if atomic.LoadInt32(&svr.refCount) == 0 {
			return
		}
		time.Sleep(time.Millisecond * 10)
	}
}

type requestCtx struct {
	svr       *Server
	regCtx    *regionCtx
	regErr    *errorpb.Error
	buf       []byte
	reader    *DBReader
	method    string
	startTime time.Time
	traces    []traceItem
}

type traceItem struct {
	event      string
	sinceStart time.Duration
}

const (
	eventReadLock       = ">RLock"
	eventReadDB         = ">RDB"
	eventBeginWriteLock = "<WLock"
	eventEndWriteLock   = ">WLock"
	eventBeginWriteDB   = "<WDB"
	eventInWriteDB      = "=WDB"
	eventEndWriteDB     = ">WDB"
	eventAcquireLatches = ">Latch"
	eventFinish         = ">Fin"
)

func (ti traceItem) String() string {
	return ti.event + ":" + ti.sinceStart.String()
}

func newRequestCtx(svr *Server, ctx *kvrpcpb.Context, method string) (*requestCtx, error) {
	atomic.AddInt32(&svr.refCount, 1)
	if atomic.LoadInt32(&svr.stopped) > 0 {
		atomic.AddInt32(&svr.refCount, -1)
		return nil, ErrRetryable("server is closed")
	}
	req := &requestCtx{
		svr:       svr,
		method:    method,
		startTime: time.Now(),
		traces:    make([]traceItem, 0, 16),
	}
	req.regCtx, req.regErr = svr.regionManager.getRegionFromCtx(ctx)
	if req.regErr != nil {
		return req, nil
	}
	req.regCtx.refCount.Add(1)
	return req, nil
}

func (req *requestCtx) trace(event string) {
	req.traces = append(req.traces, traceItem{
		event:      event,
		sinceStart: time.Since(req.startTime),
	})
}

func (req *requestCtx) traceAt(event string, t time.Time) {
	req.traces = append(req.traces, traceItem{
		event:      event,
		sinceStart: t.Sub(req.startTime),
	})
}

// For read-only requests that doesn't acquire latches, this function must be called after all locks has been checked.
func (req *requestCtx) getDBReader() *DBReader {
	if req.reader == nil {
		req.reader = req.svr.mvccStore.NewDBReader(req)
	}
	return req.reader
}

var LogTraceMS uint = 300

func (req *requestCtx) finish() {
	atomic.AddInt32(&req.svr.refCount, -1)
	if req.reader != nil {
		req.reader.Close()
	}
	if req.regCtx != nil {
		req.regCtx.refCount.Done()
	}
	req.trace(eventFinish)
	last := req.traces[len(req.traces)-1]
	if last.sinceStart > time.Millisecond*time.Duration(LogTraceMS) {
		log.Warnf("SLOW %s %#s", req.method, req.traces)
	}
}

func (svr *Server) KvGet(ctx context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvGet")
	if err != nil {
		return &kvrpcpb.GetResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.GetResponse{RegionError: reqCtx.regErr}, nil
	}
	err = svr.mvccStore.CheckKeysLock(req.GetVersion(), req.Key)
	if err != nil {
		return &kvrpcpb.GetResponse{Error: convertToKeyError(err)}, nil
	}
	reader := reqCtx.getDBReader()
	val, err := reader.Get(req.Key, req.GetVersion())
	if err != nil {
		return &kvrpcpb.GetResponse{
			Error: convertToKeyError(err),
		}, nil
	}
	return &kvrpcpb.GetResponse{
		Value: val,
	}, nil
}

func (svr *Server) KvScan(ctx context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvScan")
	if err != nil {
		return &kvrpcpb.ScanResponse{Pairs: convertToPbPairs([]Pair{{Err: err}})}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.ScanResponse{RegionError: reqCtx.regErr}, nil
	}
	if !isMvccRegion(reqCtx.regCtx) {
		return &kvrpcpb.ScanResponse{}, nil
	}
	startKey := req.GetStartKey()
	endKey := reqCtx.regCtx.rawEndKey()
	err = svr.mvccStore.CheckRangeLock(req.GetVersion(), startKey, endKey)
	if err != nil {
		return &kvrpcpb.ScanResponse{Pairs: convertToPbPairs([]Pair{{Err: err}})}, nil
	}
	reader := reqCtx.getDBReader()
	pairs := reader.Scan(startKey, endKey, int(req.GetLimit()), req.GetVersion())
	return &kvrpcpb.ScanResponse{
		Pairs: convertToPbPairs(pairs),
	}, nil
}

func (svr *Server) KvPrewrite(ctx context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvPrewrite")
	if err != nil {
		return &kvrpcpb.PrewriteResponse{Errors: convertToKeyErrors([]error{err})}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.PrewriteResponse{RegionError: reqCtx.regErr}, nil
	}
	errs := svr.mvccStore.Prewrite(reqCtx, req.Mutations, req.PrimaryLock, req.GetStartVersion(), req.GetLockTtl())
	return &kvrpcpb.PrewriteResponse{
		Errors: convertToKeyErrors(errs),
	}, nil
}

func (svr *Server) KvCommit(ctx context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvCommit")
	if err != nil {
		return &kvrpcpb.CommitResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.CommitResponse{RegionError: reqCtx.regErr}, nil
	}
	err = svr.mvccStore.Commit(reqCtx, req.Keys, req.GetStartVersion(), req.GetCommitVersion())
	return &kvrpcpb.CommitResponse{
		Error: convertToKeyError(err),
	}, nil
}

func (svr *Server) KvImport(context.Context, *kvrpcpb.ImportRequest) (*kvrpcpb.ImportResponse, error) {
	// TODO
	return &kvrpcpb.ImportResponse{}, nil
}

func (svr *Server) KvCleanup(ctx context.Context, req *kvrpcpb.CleanupRequest) (*kvrpcpb.CleanupResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvCleanup")
	if err != nil {
		return &kvrpcpb.CleanupResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.CleanupResponse{RegionError: reqCtx.regErr}, nil
	}
	err = svr.mvccStore.Cleanup(reqCtx, req.Key, req.StartVersion)
	resp := new(kvrpcpb.CleanupResponse)
	if committed, ok := err.(ErrAlreadyCommitted); ok {
		resp.CommitVersion = uint64(committed)
	} else if err != nil {
		log.Error(err)
		resp.Error = convertToKeyError(err)
	}
	return resp, nil
}

func (svr *Server) KvBatchGet(ctx context.Context, req *kvrpcpb.BatchGetRequest) (*kvrpcpb.BatchGetResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvBatchGet")
	if err != nil {
		return &kvrpcpb.BatchGetResponse{Pairs: convertToPbPairs([]Pair{{Err: err}})}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.BatchGetResponse{RegionError: reqCtx.regErr}, nil
	}
	err = svr.mvccStore.CheckKeysLock(req.GetVersion(), req.Keys...)
	if err != nil {
		return &kvrpcpb.BatchGetResponse{Pairs: convertToPbPairs([]Pair{{Err: err}})}, nil
	}
	pairs := reqCtx.getDBReader().BatchGet(req.Keys, req.GetVersion())
	return &kvrpcpb.BatchGetResponse{
		Pairs: convertToPbPairs(pairs),
	}, nil
}

func (svr *Server) KvBatchRollback(ctx context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvBatchRollback")
	if err != nil {
		return &kvrpcpb.BatchRollbackResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.BatchRollbackResponse{RegionError: reqCtx.regErr}, nil
	}
	err = svr.mvccStore.Rollback(reqCtx, req.Keys, req.StartVersion)
	if err != nil {
		return &kvrpcpb.BatchRollbackResponse{Error: convertToKeyError(err)}, nil
	}
	return &kvrpcpb.BatchRollbackResponse{}, nil
}

func (svr *Server) KvScanLock(ctx context.Context, req *kvrpcpb.ScanLockRequest) (*kvrpcpb.ScanLockResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvScanLock")
	if err != nil {
		return &kvrpcpb.ScanLockResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.ScanLockResponse{RegionError: reqCtx.regErr}, nil
	}
	log.Debug("kv scan lock")
	if !isMvccRegion(reqCtx.regCtx) {
		return &kvrpcpb.ScanLockResponse{}, nil
	}
	locks, err := svr.mvccStore.ScanLock(reqCtx, req.MaxVersion)
	return &kvrpcpb.ScanLockResponse{Error: convertToKeyError(err), Locks: locks}, nil
}

func (svr *Server) KvResolveLock(ctx context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvResolveLock")
	if err != nil {
		return &kvrpcpb.ResolveLockResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.ResolveLockResponse{RegionError: reqCtx.regErr}, nil
	}
	if !isMvccRegion(reqCtx.regCtx) {
		return &kvrpcpb.ResolveLockResponse{}, nil
	}
	resp := &kvrpcpb.ResolveLockResponse{}
	if len(req.TxnInfos) > 0 {
		for _, txnInfo := range req.TxnInfos {
			log.Debugf("kv resolve lock region:%d txn:%v", reqCtx.regCtx.meta.Id, txnInfo.Txn)
			err := svr.mvccStore.ResolveLock(reqCtx, txnInfo.Txn, txnInfo.Status)
			if err != nil {
				resp.Error = convertToKeyError(err)
				break
			}
		}
	} else {
		log.Debugf("kv resolve lock region:%d txn:%v", reqCtx.regCtx.meta.Id, req.StartVersion)
		err := svr.mvccStore.ResolveLock(reqCtx, req.StartVersion, req.CommitVersion)
		if err != nil {
			resp.Error = convertToKeyError(err)
		}
	}
	return resp, nil
}

func (svr *Server) KvGC(ctx context.Context, req *kvrpcpb.GCRequest) (*kvrpcpb.GCResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvGC")
	if err != nil {
		return &kvrpcpb.GCResponse{Error: convertToKeyError(err)}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.GCResponse{RegionError: reqCtx.regErr}, nil
	}
	log.Debug("kv GC safePoint:", extractPhysicalTime(req.SafePoint))
	if !isMvccRegion(reqCtx.regCtx) {
		return &kvrpcpb.GCResponse{}, nil
	}
	err = svr.mvccStore.GC(reqCtx, req.SafePoint)
	return &kvrpcpb.GCResponse{Error: convertToKeyError(err)}, nil
}

func (svr *Server) KvDeleteRange(ctx context.Context, req *kvrpcpb.DeleteRangeRequest) (*kvrpcpb.DeleteRangeResponse, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "KvDeleteRange")
	if err != nil {
		return &kvrpcpb.DeleteRangeResponse{Error: convertToKeyError(err).String()}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &kvrpcpb.DeleteRangeResponse{RegionError: reqCtx.regErr}, nil
	}
	if !isMvccRegion(reqCtx.regCtx) {
		return &kvrpcpb.DeleteRangeResponse{}, nil
	}
	err = svr.mvccStore.DeleteRange(reqCtx, req.StartKey, req.EndKey)
	if err != nil {
		log.Error(err)
	}
	return &kvrpcpb.DeleteRangeResponse{}, nil
}

// RawKV commands.
func (svr *Server) RawGet(context.Context, *kvrpcpb.RawGetRequest) (*kvrpcpb.RawGetResponse, error) {
	return &kvrpcpb.RawGetResponse{}, nil
}

func (svr *Server) RawPut(context.Context, *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	return &kvrpcpb.RawPutResponse{}, nil
}

func (svr *Server) RawDelete(context.Context, *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	return &kvrpcpb.RawDeleteResponse{}, nil
}

func (svr *Server) RawScan(context.Context, *kvrpcpb.RawScanRequest) (*kvrpcpb.RawScanResponse, error) {
	return &kvrpcpb.RawScanResponse{}, nil
}

func (svr *Server) RawBatchDelete(context.Context, *kvrpcpb.RawBatchDeleteRequest) (*kvrpcpb.RawBatchDeleteResponse, error) {
	return &kvrpcpb.RawBatchDeleteResponse{}, nil
}

func (svr *Server) RawBatchGet(context.Context, *kvrpcpb.RawBatchGetRequest) (*kvrpcpb.RawBatchGetResponse, error) {
	return &kvrpcpb.RawBatchGetResponse{}, nil
}

func (svr *Server) RawBatchPut(context.Context, *kvrpcpb.RawBatchPutRequest) (*kvrpcpb.RawBatchPutResponse, error) {
	return &kvrpcpb.RawBatchPutResponse{}, nil
}

func (svr *Server) RawBatchScan(context.Context, *kvrpcpb.RawBatchScanRequest) (*kvrpcpb.RawBatchScanResponse, error) {
	return &kvrpcpb.RawBatchScanResponse{}, nil
}

func (svr *Server) RawDeleteRange(context.Context, *kvrpcpb.RawDeleteRangeRequest) (*kvrpcpb.RawDeleteRangeResponse, error) {
	return &kvrpcpb.RawDeleteRangeResponse{}, nil
}

// SQL push down commands.
func (svr *Server) Coprocessor(ctx context.Context, req *coprocessor.Request) (*coprocessor.Response, error) {
	reqCtx, err := newRequestCtx(svr, req.Context, "Coprocessor")
	if err != nil {
		return &coprocessor.Response{OtherError: convertToKeyError(err).String()}, nil
	}
	defer reqCtx.finish()
	if reqCtx.regErr != nil {
		return &coprocessor.Response{RegionError: reqCtx.regErr}, nil
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return svr.handleCopDAGRequest(reqCtx, req), nil
	case kv.ReqTypeAnalyze:
		return svr.handleCopAnalyzeRequest(reqCtx, req), nil
	}
	return nil, errors.Errorf("unsupported request type %d", req.GetTp())
}

func (svr *Server) CoprocessorStream(*coprocessor.Request, tikvpb.Tikv_CoprocessorStreamServer) error {
	// TODO
	return nil
}

// Raft commands (tikv <-> tikv).
func (svr *Server) Raft(tikvpb.Tikv_RaftServer) error {
	return nil
}
func (svr *Server) Snapshot(tikvpb.Tikv_SnapshotServer) error {
	return nil
}

// Region commands.
func (svr *Server) SplitRegion(ctx context.Context, req *kvrpcpb.SplitRegionRequest) (*kvrpcpb.SplitRegionResponse, error) {
	// TODO
	return &kvrpcpb.SplitRegionResponse{}, nil
}

// transaction debugger commands.
func (svr *Server) MvccGetByKey(context.Context, *kvrpcpb.MvccGetByKeyRequest) (*kvrpcpb.MvccGetByKeyResponse, error) {
	// TODO
	return nil, nil
}

func (svr *Server) MvccGetByStartTs(context.Context, *kvrpcpb.MvccGetByStartTsRequest) (*kvrpcpb.MvccGetByStartTsResponse, error) {
	// TODO
	return nil, nil
}

func convertToKeyError(err error) *kvrpcpb.KeyError {
	if err == nil {
		return nil
	}
	if locked, ok := errors.Cause(err).(*ErrLocked); ok {
		return &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				Key:         locked.Key,
				PrimaryLock: locked.Primary,
				LockVersion: locked.StartTS,
				LockTtl:     locked.TTL,
			},
		}
	}
	if retryable, ok := errors.Cause(err).(ErrRetryable); ok {
		return &kvrpcpb.KeyError{
			Retryable: retryable.Error(),
		}
	}
	return &kvrpcpb.KeyError{
		Abort: err.Error(),
	}
}

func convertToKeyErrors(errs []error) []*kvrpcpb.KeyError {
	var keyErrors []*kvrpcpb.KeyError
	for _, err := range errs {
		if err != nil {
			keyErrors = append(keyErrors, convertToKeyError(err))
		}
	}
	return keyErrors
}

func convertToPbPairs(pairs []Pair) []*kvrpcpb.KvPair {
	kvPairs := make([]*kvrpcpb.KvPair, 0, len(pairs))
	for _, p := range pairs {
		var kvPair *kvrpcpb.KvPair
		if p.Err == nil {
			kvPair = &kvrpcpb.KvPair{
				Key:   p.Key,
				Value: p.Value,
			}
		} else {
			kvPair = &kvrpcpb.KvPair{
				Error: convertToKeyError(p.Err),
			}
		}
		kvPairs = append(kvPairs, kvPair)
	}
	return kvPairs
}

func isMvccRegion(regCtx *regionCtx) bool {
	if len(regCtx.startKey) == 0 {
		return false
	}
	first := regCtx.startKey[0]
	return first == 't' || first == 'm'
}
