package store

import (
	"bytes"
	"context"
	"io"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-car"
	carutil "github.com/ipld/go-car/util"
	carv2 "github.com/ipld/go-car/v2"
	mh "github.com/multiformats/go-multihash"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	bstore "github.com/filecoin-project/lotus/blockstore"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors/builtin"
	"github.com/filecoin-project/lotus/chain/types"
)

func (cs *ChainStore) UnionStore() bstore.Blockstore {
	return bstore.Union(cs.stateBlockstore, cs.chainBlockstore)
}

func (cs *ChainStore) Export(ctx context.Context, ts *types.TipSet, inclRecentRoots abi.ChainEpoch, skipOldMsgs bool, w io.Writer) error {
	h := &car.CarHeader{
		Roots:   ts.Cids(),
		Version: 1,
	}

	if err := car.WriteHeader(h, w); err != nil {
		return xerrors.Errorf("failed to write car header: %s", err)
	}

	unionBs := cs.UnionStore()
	return cs.WalkSnapshot(ctx, ts, inclRecentRoots, skipOldMsgs, true, func(c cid.Cid) error {
		blk, err := unionBs.Get(ctx, c)
		if err != nil {
			return xerrors.Errorf("writing object to car, bs.Get: %w", err)
		}

		if err := carutil.LdWrite(w, c.Bytes(), blk.RawData()); err != nil {
			return xerrors.Errorf("failed to write block to car output: %w", err)
		}

		return nil
	})
}

func (cs *ChainStore) Import(ctx context.Context, r io.Reader) (*types.TipSet, error) {
	// TODO: writing only to the state blockstore is incorrect.
	//  At this time, both the state and chain blockstores are backed by the
	//  universal store. When we physically segregate the stores, we will need
	//  to route state objects to the state blockstore, and chain objects to
	//  the chain blockstore.

	br, err := carv2.NewBlockReader(r)
	if err != nil {
		return nil, xerrors.Errorf("loadcar failed: %w", err)
	}

	s := cs.StateBlockstore()

	parallelPuts := 5
	putThrottle := make(chan error, parallelPuts)
	for i := 0; i < parallelPuts; i++ {
		putThrottle <- nil
	}

	var buf []blocks.Block
	for {
		blk, err := br.Next()
		if err != nil {
			if err == io.EOF {
				if len(buf) > 0 {
					if err := s.PutMany(ctx, buf); err != nil {
						return nil, err
					}
				}

				break
			}
			return nil, err
		}

		buf = append(buf, blk)

		if len(buf) > 1000 {
			if lastErr := <-putThrottle; lastErr != nil { // consume one error to have the right to add one
				return nil, lastErr
			}

			go func(buf []blocks.Block) {
				putThrottle <- s.PutMany(ctx, buf)
			}(buf)
			buf = nil
		}
	}

	// check errors
	for i := 0; i < parallelPuts; i++ {
		if lastErr := <-putThrottle; lastErr != nil {
			return nil, lastErr
		}
	}

	root, err := cs.LoadTipSet(ctx, types.NewTipSetKey(br.Roots...))
	if err != nil {
		return nil, xerrors.Errorf("failed to load root tipset from chainfile: %w", err)
	}

	return root, nil
}

func (cs *ChainStore) WalkSnapshot(ctx context.Context, ts *types.TipSet, inclRecentRoots abi.ChainEpoch, skipOldMsgs, skipMsgReceipts bool, cb func(cid.Cid) error) error {
	if ts == nil {
		ts = cs.GetHeaviestTipSet()
	}

	seen := cid.NewSet()
	walked := cid.NewSet()

	blocksToWalk := ts.Cids()
	currentMinHeight := ts.Height()

	walkChain := func(blk cid.Cid) error {
		if !seen.Visit(blk) {
			return nil
		}

		if err := cb(blk); err != nil {
			return err
		}

		data, err := cs.chainBlockstore.Get(ctx, blk)
		if err != nil {
			return xerrors.Errorf("getting block: %w", err)
		}

		var b types.BlockHeader
		if err := b.UnmarshalCBOR(bytes.NewBuffer(data.RawData())); err != nil {
			return xerrors.Errorf("unmarshaling block header (cid=%s): %w", blk, err)
		}

		if currentMinHeight > b.Height {
			currentMinHeight = b.Height
			if currentMinHeight%builtin.EpochsInDay == 0 {
				log.Infow("export", "height", currentMinHeight)
			}
		}

		var cids []cid.Cid
		if !skipOldMsgs || b.Height > ts.Height()-inclRecentRoots {
			if walked.Visit(b.Messages) {
				mcids, err := recurseLinks(ctx, cs.chainBlockstore, walked, b.Messages, []cid.Cid{b.Messages})
				if err != nil {
					return xerrors.Errorf("recursing messages failed: %w", err)
				}
				cids = mcids
			}
		}

		if b.Height > 0 {
			for _, p := range b.Parents {
				blocksToWalk = append(blocksToWalk, p)
			}
		} else {
			// include the genesis block
			cids = append(cids, b.Parents...)
		}

		out := cids

		if b.Height == 0 || b.Height > ts.Height()-inclRecentRoots {
			if walked.Visit(b.ParentStateRoot) {
				cids, err := recurseLinks(ctx, cs.stateBlockstore, walked, b.ParentStateRoot, []cid.Cid{b.ParentStateRoot})
				if err != nil {
					return xerrors.Errorf("recursing genesis state failed: %w", err)
				}

				out = append(out, cids...)
			}

			if !skipMsgReceipts && walked.Visit(b.ParentMessageReceipts) {
				out = append(out, b.ParentMessageReceipts)
			}
		}

		for _, c := range out {
			if seen.Visit(c) {
				prefix := c.Prefix()

				// Don't include identity CIDs.
				if prefix.MhType == mh.IDENTITY {
					continue
				}

				// We only include raw and dagcbor, for now.
				// Raw for "code" CIDs.
				switch prefix.Codec {
				case cid.Raw, cid.DagCBOR:
				default:
					continue
				}

				if err := cb(c); err != nil {
					return err
				}

			}
		}

		return nil
	}

	log.Infow("export started")
	exportStart := build.Clock.Now()

	for len(blocksToWalk) > 0 {
		next := blocksToWalk[0]
		blocksToWalk = blocksToWalk[1:]
		if err := walkChain(next); err != nil {
			return xerrors.Errorf("walk chain failed: %w", err)
		}
	}

	log.Infow("export finished", "duration", build.Clock.Now().Sub(exportStart).Seconds())

	return nil
}

func recurseLinks(ctx context.Context, bs bstore.Blockstore, walked *cid.Set, root cid.Cid, in []cid.Cid) ([]cid.Cid, error) {
	if root.Prefix().Codec != cid.DagCBOR {
		return in, nil
	}

	data, err := bs.Get(ctx, root)
	if err != nil {
		return nil, xerrors.Errorf("recurse links get (%s) failed: %w", root, err)
	}

	var rerr error
	err = cbg.ScanForLinks(bytes.NewReader(data.RawData()), func(c cid.Cid) {
		if rerr != nil {
			// No error return on ScanForLinks :(
			return
		}

		// traversed this already...
		if !walked.Visit(c) {
			return
		}

		in = append(in, c)
		var err error
		in, err = recurseLinks(ctx, bs, walked, c, in)
		if err != nil {
			rerr = err
		}
	})
	if err != nil {
		return nil, xerrors.Errorf("scanning for links failed: %w", err)
	}

	return in, rerr
}
