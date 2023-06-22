package piecedirectory

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/filecoin-project/boost/piecedirectory/types"
	bdclient "github.com/filecoin-project/boostd-data/client"
	"github.com/filecoin-project/boostd-data/model"
	"github.com/filecoin-project/boostd-data/shared/tracing"
	bdtypes "github.com/filecoin-project/boostd-data/svc/types"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/markets/dagstore"
	"github.com/hashicorp/go-multierror"
	"github.com/ipfs/go-cid"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	format "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipld/go-car/util"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/ipld/go-car/v2/blockstore"
	carindex "github.com/ipld/go-car/v2/index"
	"github.com/multiformats/go-multihash"
	mh "github.com/multiformats/go-multihash"
)

var log = logging.Logger("piecedirectory")

type PieceDirectory struct {
	store              *bdclient.Store
	pieceReader        types.PieceReader
	addIdxThrottleSize int

	ctx context.Context

	addIdxLk  sync.Mutex
	addIdxOps []*addIdxOperation
}

func NewPieceDirectory(store *bdclient.Store, pr types.PieceReader, addIndexThrottleSize int) *PieceDirectory {
	return &PieceDirectory{
		store:              store,
		pieceReader:        pr,
		addIdxThrottleSize: addIndexThrottleSize,
	}
}

func (ps *PieceDirectory) Start(ctx context.Context) {
	ps.ctx = ctx
}

func (ps *PieceDirectory) FlaggedPiecesList(ctx context.Context, filter *bdtypes.FlaggedPiecesListFilter, cursor *time.Time, offset int, limit int) ([]model.FlaggedPiece, error) {
	return ps.store.FlaggedPiecesList(ctx, filter, cursor, offset, limit)
}

func (ps *PieceDirectory) FlaggedPiecesCount(ctx context.Context, filter *bdtypes.FlaggedPiecesListFilter) (int, error) {
	return ps.store.FlaggedPiecesCount(ctx, filter)
}

func (ps *PieceDirectory) PiecesCount(ctx context.Context) (int, error) {
	return ps.store.PiecesCount(ctx)
}

// Get all metadata about a particular piece
func (ps *PieceDirectory) GetPieceMetadata(ctx context.Context, pieceCid cid.Cid) (types.PieceDirMetadata, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_piece_metadata")
	defer span.End()

	// Get the piece metadata from the DB
	log.Debugw("piece metadata: get", "pieceCid", pieceCid)
	md, err := ps.store.GetPieceMetadata(ctx, pieceCid)
	if err != nil {
		return types.PieceDirMetadata{}, err
	}

	// Check if this process is currently indexing the piece
	log.Debugw("piece metadata: get indexing status", "pieceCid", pieceCid)
	ops := ps.AddIndexOperations()
	indexing := false
	for _, op := range ops {
		if op.PieceCid == pieceCid {
			indexing = true
		}
	}

	// Return the db piece metadata along with the indexing flag
	log.Debugw("piece metadata: get complete", "pieceCid", pieceCid)
	return types.PieceDirMetadata{
		Metadata: md,
		Indexing: indexing,
	}, nil
}

// Get the list of deals (and the sector the data is in) for a particular piece
func (ps *PieceDirectory) GetPieceDeals(ctx context.Context, pieceCid cid.Cid) ([]model.DealInfo, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_piece_deals")
	defer span.End()

	deals, err := ps.store.GetPieceDeals(ctx, pieceCid)
	if err != nil {
		return nil, fmt.Errorf("listing deals for piece %s: %w", pieceCid, err)
	}

	return deals, nil
}

func (ps *PieceDirectory) GetOffsetSize(ctx context.Context, pieceCid cid.Cid, hash mh.Multihash) (*model.OffsetSize, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_offset")
	defer span.End()

	return ps.store.GetOffsetSize(ctx, pieceCid, hash)
}

func (ps *PieceDirectory) AddDealForPiece(ctx context.Context, pieceCid cid.Cid, dealInfo model.DealInfo) error {
	log.Debugw("add deal for piece", "piececid", pieceCid, "uuid", dealInfo.DealUuid)

	ctx, span := tracing.Tracer.Start(ctx, "pm.add_deal_for_piece")
	defer span.End()

	// Check if the indexes have already been added
	isIndexed, err := ps.store.IsIndexed(ctx, pieceCid)
	if err != nil {
		return err
	}

	if !isIndexed {
		// Perform indexing of piece
		op := ps.addIndexForPieceThrottled(pieceCid, dealInfo)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-op.done:
		}
		if op.err != nil {
			return fmt.Errorf("adding index for piece %s: %w", pieceCid, op.err)
		}
	}

	// Add deal to list of deals for this piece
	if err := ps.store.AddDealForPiece(ctx, pieceCid, dealInfo); err != nil {
		return fmt.Errorf("saving deal %s to store: %w", dealInfo.DealUuid, err)
	}

	return nil
}

func (ps *PieceDirectory) AddIndexOperations() []AddIdxState {
	ops := make([]AddIdxState, 0, len(ps.addIdxOps))

	ps.addIdxLk.Lock()
	defer ps.addIdxLk.Unlock()

	for _, o := range ps.addIdxOps {
		ops = append(ops, o.State())
	}

	return ops
}

type addIdxOperation struct {
	pieceCid cid.Cid
	dealInfo model.DealInfo
	done     chan struct{}

	lk       sync.RWMutex
	started  bool
	progress float64
	err      error
}

type AddIdxState struct {
	PieceCid cid.Cid
	Progress float64
	Err      error
}

func (o *addIdxOperation) State() AddIdxState {
	o.lk.Lock()
	defer o.lk.Unlock()
	return AddIdxState{
		PieceCid: o.pieceCid,
		Progress: o.progress,
		Err:      o.err,
	}
}

func (o *addIdxOperation) setProgress(progress float64) {
	o.lk.Lock()
	defer o.lk.Unlock()
	o.progress = progress
}

func (o *addIdxOperation) setErr(err error) {
	o.lk.Lock()
	defer o.lk.Unlock()
	o.err = err
}

func (ps *PieceDirectory) addIndexForPieceThrottled(pieceCid cid.Cid, dealInfo model.DealInfo) *addIdxOperation {
	ps.addIdxLk.Lock()
	defer ps.addIdxLk.Unlock()

	// Check if there is already an add index operation in progress for the
	// given piece cid. If not, create a new one.
	for _, op := range ps.addIdxOps {
		if op.pieceCid == pieceCid {
			log.Debugw("add index: operation already in progress", "pieceCid", pieceCid)
			return op
		}
	}

	log.Debugw("add index: new operation", "pieceCid", pieceCid)
	op := &addIdxOperation{pieceCid: pieceCid, dealInfo: dealInfo, done: make(chan struct{})}
	ps.addIdxOps = append(ps.addIdxOps, op)

	var startNextOp func()
	runOp := func(o *addIdxOperation) {
		log.Debugw("add index: start", "pieceCid", pieceCid)

		// Add index for piece, and record progress
		ps.addIndexForPiece(ps.ctx, o)

		// The add index operation has completed. Clean it up from the array.
		ps.addIdxLk.Lock()
		defer ps.addIdxLk.Unlock()

		found := false
		for i, currop := range ps.addIdxOps {
			if currop.pieceCid == o.pieceCid {
				found = true
			}
			if found && i < len(ps.addIdxOps)-1 {
				ps.addIdxOps[i] = ps.addIdxOps[i+1]
			}
		}
		ps.addIdxOps = ps.addIdxOps[:len(ps.addIdxOps)-1]

		// If there are enough open slots in the throttle, start the next operation
		startNextOp()
	}

	startNextOp = func() {
		// If there is another operation waiting to run, and there are enough
		// open slots in the throttle, start the next operation
		for i := 0; i < ps.addIdxThrottleSize && i < len(ps.addIdxOps); i++ {
			nextOp := ps.addIdxOps[i]
			if !nextOp.started {
				nextOp.started = true
				go runOp(nextOp)
			}
		}
	}

	// If there are enough open slots in the throttle, start the operation
	startNextOp()

	return op
}

func (ps *PieceDirectory) addIndexForPiece(ctx context.Context, op *addIdxOperation) {
	defer close(op.done)

	pieceCid := op.pieceCid
	dealInfo := op.dealInfo

	// Get a reader over the piece data
	log.Debugw("add index: get index", "pieceCid", pieceCid)
	reader, err := ps.pieceReader.GetReader(ctx, dealInfo.SectorID, dealInfo.PieceOffset, dealInfo.PieceLength)
	if err != nil {
		op.setErr(fmt.Errorf("getting reader over piece %s: %w", pieceCid, err))
		return
	}
	op.setProgress(0.1)

	// Iterate over all the blocks in the piece to extract the index records
	log.Debugw("add index: read index", "pieceCid", pieceCid)
	recs := make([]model.Record, 0)
	opts := []carv2.Option{carv2.ZeroLengthSectionAsEOF(true)}
	blockReader, err := carv2.NewBlockReader(reader, opts...)
	if err != nil {
		op.setErr(fmt.Errorf("getting block reader over piece %s: %w", pieceCid, err))
		return
	}

	blockMetadata, err := blockReader.SkipNext()
	for err == nil {
		recs = append(recs, model.Record{
			Cid: blockMetadata.Cid,
			OffsetSize: model.OffsetSize{
				Offset: blockMetadata.Offset,
				Size:   blockMetadata.Size,
			},
		})

		blockMetadata, err = blockReader.SkipNext()
	}
	if !errors.Is(err, io.EOF) {
		op.setErr((fmt.Errorf("generating index for piece %s: %w", pieceCid, err)))
		return
	}
	op.setProgress(0.2)

	// Add mh => piece index to store: "which piece contains the multihash?"
	// Add mh => offset index to store: "what is the offset of the multihash within the piece?"
	log.Debugw("add index: store index in local index directory", "pieceCid", pieceCid)
	addidxch := ps.store.AddIndex(ctx, pieceCid, recs, true)
	for resp := range addidxch {
		if resp.Err != "" {
			op.setErr((fmt.Errorf("adding CAR index for piece %s: %s", pieceCid, resp.Err)))
			return
		}
		op.setProgress(0.2 + resp.Progress*0.8)
	}
}

type BuildIndexProgress struct {
	Progress float64
	Error    string
}

func (ps *PieceDirectory) BuildIndexForPiece(ctx context.Context, pieceCid cid.Cid) <-chan BuildIndexProgress {
	ctx, span := tracing.Tracer.Start(ctx, "pm.build_index_for_piece")
	defer span.End()

	log.Debugw("build index: get piece deals", "pieceCid", pieceCid)

	ch := make(chan BuildIndexProgress, 256)

	if ctx.Err() != nil {
		ch <- BuildIndexProgress{Error: "context already cancelled"}
		close(ch)
		return ch
	}

	dls, err := ps.GetPieceDeals(ctx, pieceCid)
	if err != nil {
		ch <- BuildIndexProgress{Error: fmt.Sprintf("getting piece deals: %s", err)}
		close(ch)
		return ch
	}

	if len(dls) == 0 {
		log.Debugw("build index: no deals found for piece", "pieceCid", pieceCid)
		ch <- BuildIndexProgress{Error: fmt.Sprintf("getting piece deals: no deals found for piece")}
		close(ch)
		return ch
	}

	// Send progress updates on the channel every second
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer close(ch)

		op := ps.addIndexForPieceThrottled(pieceCid, dls[0])
		for {
			select {
			case <-ctx.Done():
				ch <- BuildIndexProgress{Error: "context cancelled"}
				return
			case <-op.done:
				state := op.State()
				errstr := ""
				if state.Err != nil {
					errstr = state.Err.Error()
				}
				ch <- BuildIndexProgress{Error: errstr, Progress: state.Progress}
				return
			case <-ticker.C:
				ch <- BuildIndexProgress{Progress: op.State().Progress}
			}
		}
	}()

	return ch
}

func (ps *PieceDirectory) RemoveDealForPiece(ctx context.Context, pieceCid cid.Cid, dealUuid string) error {
	ctx, span := tracing.Tracer.Start(ctx, "pm.delete_deal_for_piece")
	defer span.End()

	//Delete deal from list of deals for this piece
	//It removes metadata and indexes if []deal is empty
	err := ps.store.RemoveDealForPiece(ctx, pieceCid, dealUuid)
	if err != nil {
		return fmt.Errorf("deleting deal from piece metadata: %w", err)
	}
	return nil
}

//func (ps *piecedirectory) deleteIndexForPiece(pieceCid cid.Cid) interface{} {
// TODO: Maybe mark for GC instead of deleting immediately

// Delete mh => offset index from store
//err := ps.carIndex.Delete(pieceCid)
//if err != nil {
//err = fmt.Errorf("deleting CAR index for piece %s: %w", pieceCid, err)
//}

//// Delete mh => piece index from store
//if mherr := ps.mhToPieceIndex.Delete(pieceCid); mherr != nil {
//err = multierror.Append(fmt.Errorf("deleting cid index for piece %s: %w", pieceCid, mherr))
//}
//return err
//return nil
//}

// Used internally, and also by HTTP retrieval
func (ps *PieceDirectory) GetPieceReader(ctx context.Context, pieceCid cid.Cid) (types.SectionReader, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_piece_reader")
	defer span.End()

	// Get all deals containing this piece
	deals, err := ps.GetPieceDeals(ctx, pieceCid)
	if err != nil {
		return nil, fmt.Errorf("getting piece deals: %w", err)
	}

	if len(deals) == 0 {
		return nil, fmt.Errorf("no deals found for piece cid %s: %w", pieceCid, err)
	}

	// For each deal, try to read an unsealed copy of the data from the sector
	// it is stored in
	var merr error
	for i, dl := range deals {
		reader, err := ps.pieceReader.GetReader(ctx, dl.SectorID, dl.PieceOffset, dl.PieceLength)
		if err != nil {
			// TODO: log error
			if i < 3 {
				merr = multierror.Append(merr, err)
			}
			continue
		}

		return reader, nil
	}

	return nil, merr
}

// Get all pieces that contain a multihash (used when retrieving by payload CID)
func (ps *PieceDirectory) PiecesContainingMultihash(ctx context.Context, m mh.Multihash) ([]cid.Cid, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.pieces_containing_multihash")
	defer span.End()

	return ps.store.PiecesContainingMultihash(ctx, m)
}

func (ps *PieceDirectory) GetIterableIndex(ctx context.Context, pieceCid cid.Cid) (carindex.IterableIndex, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_iterable_index")
	defer span.End()

	idx, err := ps.store.GetIndex(ctx, pieceCid)
	if err != nil {
		return nil, err
	}

	switch concrete := idx.(type) {
	case carindex.IterableIndex:
		return concrete, nil
	default:
		return nil, fmt.Errorf("expected index to be MultihashIndexSorted but got %T", idx)
	}
}

// Get a block (used by Bitswap retrieval)
func (ps *PieceDirectory) BlockstoreGet(ctx context.Context, c cid.Cid) ([]byte, error) {
	// TODO: use caching to make this efficient for repeated Gets against the same piece
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_block")
	defer span.End()

	// Get the pieces that contain the cid
	pieces, err := ps.PiecesContainingMultihash(ctx, c.Hash())

	// Check if it's an identity cid, if it is, return its digest
	if err != nil {
		digest, ok, err := isIdentity(c)
		if err == nil && ok {
			return digest, nil
		}
		return nil, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	if len(pieces) == 0 {
		return nil, fmt.Errorf("no pieces with cid %s found", c)
	}

	// Get a reader over one of the pieces and extract the block data
	var merr error
	for i, pieceCid := range pieces {
		data, err := func() ([]byte, error) {
			// Get a reader over the piece data
			reader, err := ps.GetPieceReader(ctx, pieceCid)
			if err != nil {
				return nil, fmt.Errorf("getting piece reader: %w", err)
			}
			defer reader.Close()

			// Get the offset of the block within the piece (CAR file)
			offsetSize, err := ps.GetOffsetSize(ctx, pieceCid, c.Hash())
			if err != nil {
				return nil, fmt.Errorf("getting offset/size for cid %s in piece %s: %w", c, pieceCid, err)
			}

			// Seek to the block offset
			_, err = reader.Seek(int64(offsetSize.Offset), io.SeekStart)
			if err != nil {
				return nil, fmt.Errorf("seeking to offset %d in piece reader: %w", int64(offsetSize.Offset), err)
			}

			// Read the block data
			_, data, err := util.ReadNode(bufio.NewReader(reader))
			if err != nil {
				return nil, fmt.Errorf("reading data for block %s from reader for piece %s: %w", c, pieceCid, err)
			}
			return data, nil
		}()
		if err != nil {
			if i < 3 {
				merr = multierror.Append(merr, err)
			}
			continue
		}
		return data, nil
	}

	return nil, merr
}

func (ps *PieceDirectory) BlockstoreGetSize(ctx context.Context, c cid.Cid) (int, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_block_size")
	defer span.End()

	// Get the pieces that contain the cid
	pieces, err := ps.PiecesContainingMultihash(ctx, c.Hash())
	if err != nil {
		return 0, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	if len(pieces) == 0 {
		// We must return ipld ErrNotFound here because that's the only type
		// that bitswap interprets as a not found error. All other error types
		// are treated as general errors.
		return 0, format.ErrNotFound{Cid: c}
	}

	// Get the size of the block from the first piece (should be the same for
	// any piece)
	offsetSize, err := ps.GetOffsetSize(ctx, pieces[0], c.Hash())
	if err != nil {
		return 0, fmt.Errorf("getting size of cid %s in piece %s: %w", c, pieces[0], err)
	}

	if offsetSize.Size > 0 {
		return int(offsetSize.Size), nil
	}

	// Indexes imported from the DAG store do not have block size information
	// (they only have offset information). Check if the block size is zero
	// because the index is incomplete.
	isComplete, err := ps.store.IsCompleteIndex(ctx, pieces[0])
	if err != nil {
		return 0, fmt.Errorf("getting index complete status for piece %s: %w", pieces[0], err)
	}

	if isComplete {
		// The deal index is complete, so it must be a zero-sized block.
		// A zero-sized block is unusual, but possible.
		return int(offsetSize.Size), nil
	}

	// The index is incomplete, so re-build the index on the fly
	ch := ps.BuildIndexForPiece(ctx, pieces[0])
	for progress := range ch {
		if progress.Error != "" {
			return 0, fmt.Errorf("re-building index for piece %s: %s", pieces[0], progress.Error)
		}
	}

	// Now get the size again
	offsetSize, err = ps.GetOffsetSize(ctx, pieces[0], c.Hash())
	if err != nil {
		return 0, fmt.Errorf("getting size of cid %s in piece %s: %w", c, pieces[0], err)
	}

	return int(offsetSize.Size), nil
}

func (ps *PieceDirectory) BlockstoreHas(ctx context.Context, c cid.Cid) (bool, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.has_block")
	defer span.End()

	// Get the pieces that contain the cid
	pieces, err := ps.PiecesContainingMultihash(ctx, c.Hash())
	if err != nil {
		return false, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	return len(pieces) > 0, nil
}

// Get a blockstore over a piece (used by Graphsync retrieval)
func (ps *PieceDirectory) GetBlockstore(ctx context.Context, pieceCid cid.Cid) (bstore.Blockstore, error) {
	ctx, span := tracing.Tracer.Start(ctx, "pm.get_blockstore")
	defer span.End()

	// Get a reader over the piece
	reader, err := ps.GetPieceReader(ctx, pieceCid)
	if err != nil {
		return nil, fmt.Errorf("getting piece reader for piece %s: %w", pieceCid, err)
	}

	// Get an index for the piece
	idx, err := ps.GetIterableIndex(ctx, pieceCid)
	if err != nil {
		return nil, fmt.Errorf("getting index for piece %s: %w", pieceCid, err)
	}

	// process index and store entries
	// Create a blockstore from the index and the piece reader
	bs, err := blockstore.NewReadOnly(reader, idx, carv2.ZeroLengthSectionAsEOF(true))
	if err != nil {
		return nil, fmt.Errorf("creating blockstore for piece %s: %w", pieceCid, err)
	}

	return bs, nil
}

type SectorAccessorAsPieceReader struct {
	dagstore.SectorAccessor
}

func (s *SectorAccessorAsPieceReader) GetReader(ctx context.Context, id abi.SectorNumber, offset abi.PaddedPieceSize, length abi.PaddedPieceSize) (types.SectionReader, error) {
	ctx, span := tracing.Tracer.Start(ctx, "sealer.get_reader")
	defer span.End()

	isUnsealed, err := s.SectorAccessor.IsUnsealed(ctx, id, offset.Unpadded(), length.Unpadded())
	if err != nil {
		return nil, fmt.Errorf("checking unsealed state of sector %d: %w", id, err)
	}

	if !isUnsealed {
		return nil, fmt.Errorf("getting reader over sector %d: %w", id, types.ErrSealed)
	}

	r, err := s.SectorAccessor.UnsealSectorAt(ctx, id, offset.Unpadded(), length.Unpadded())
	if err != nil {
		return nil, fmt.Errorf("getting reader over sector %d: %w", id, err)
	}

	return r, nil
}

func isIdentity(c cid.Cid) (digest []byte, ok bool, err error) {
	dmh, err := multihash.Decode(c.Hash())
	if err != nil {
		return nil, false, err
	}
	ok = dmh.Code == multihash.IDENTITY
	digest = dmh.Digest
	return digest, ok, nil
}
