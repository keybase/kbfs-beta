package libkbfs

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/keybase/client/go/libkb"
	"golang.org/x/net/context"
)

func totalBlockRefs(m map[BlockID]map[BlockRefNonce]blockRefLocalStatus) int {
	n := 0
	for _, refs := range m {
		n += len(refs)
	}
	return n
}

// Test that quota reclamation works for a simple case where the user
// does a few updates, then lets quota reclamation run, and we make
// sure that all historical blocks have been deleted.
func TestQuotaReclamationSimple(t *testing.T) {
	var userName libkb.NormalizedUsername = "test_user"
	config, _, ctx := kbfsOpsInitNoMocks(t, userName)
	defer CheckConfigAndShutdown(t, config)

	now := time.Now()
	clock := &TestClock{now}
	config.SetClock(clock)

	kbfsOps := config.KBFSOps()
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNode(ctx, userName.String(), false, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	_, _, err = kbfsOps.CreateDir(ctx, rootNode, "a")
	if err != nil {
		t.Fatalf("Couldn't create dir: %v", err)
	}
	err = kbfsOps.RemoveDir(ctx, rootNode, "a")
	if err != nil {
		t.Fatalf("Couldn't remove dir: %v", err)
	}

	// Wait for outstanding archives
	err = kbfsOps.SyncFromServer(ctx, rootNode.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	// Make sure no blocks are deleted before there's a new-enough update.
	bserverLocal, ok := config.BlockServer().(*BlockServerLocal)
	if !ok {
		t.Fatalf("Bad block server")
	}
	preQR1Blocks, err := bserverLocal.getAll(rootNode.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	ops := kbfsOps.(*KBFSOpsStandard).getOpsByNode(rootNode)
	ops.fbm.forceQuotaReclamation()
	err = ops.fbm.waitForQuotaReclamations(ctx)
	if err != nil {
		t.Fatalf("Couldn't wait for QR: %v", err)
	}

	postQR1Blocks, err := bserverLocal.getAll(rootNode.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	if !reflect.DeepEqual(preQR1Blocks, postQR1Blocks) {
		t.Fatalf("Blocks deleted too early (%v vs %v)!",
			preQR1Blocks, postQR1Blocks)
	}

	// Increase the time and make a new revision, but don't run quota
	// reclamation yet.
	clock.T = now.Add(2 * config.QuotaReclamationMinUnrefAge())
	_, _, err = kbfsOps.CreateDir(ctx, rootNode, "b")
	if err != nil {
		t.Fatalf("Couldn't create dir: %v", err)
	}

	preQR2Blocks, err := bserverLocal.getAll(rootNode.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	ops.fbm.forceQuotaReclamation()
	err = ops.fbm.waitForQuotaReclamations(ctx)
	if err != nil {
		t.Fatalf("Couldn't wait for QR: %v", err)
	}

	postQR2Blocks, err := bserverLocal.getAll(rootNode.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	if pre, post := totalBlockRefs(preQR2Blocks),
		totalBlockRefs(postQR2Blocks); post >= pre {
		t.Errorf("Blocks didn't shrink after reclamation: pre: %d, post %d",
			pre, post)
	}
}

// Test that a single quota reclamation run doesn't try to reclaim too
// much quota at once.
func TestQuotaReclamationIncrementalReclamation(t *testing.T) {
	var userName libkb.NormalizedUsername = "test_user"
	config, _, ctx := kbfsOpsInitNoMocks(t, userName)
	defer CheckConfigAndShutdown(t, config)

	now := time.Now()
	clock := &TestClock{now}
	config.SetClock(clock)

	kbfsOps := config.KBFSOps()
	rootNode, _, err :=
		kbfsOps.GetOrCreateRootNode(ctx, userName.String(), false, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	// Do a bunch of operations.
	for i := 0; i < numPointersPerGCThreshold; i++ {
		_, _, err = kbfsOps.CreateDir(ctx, rootNode, "a")
		if err != nil {
			t.Fatalf("Couldn't create dir: %v", err)
		}
		err = kbfsOps.RemoveDir(ctx, rootNode, "a")
		if err != nil {
			t.Fatalf("Couldn't remove dir: %v", err)
		}
	}

	// Increase the time, and make sure that there is still more than
	// one block in the history
	clock.T = now.Add(2 * config.QuotaReclamationMinUnrefAge())

	// Run it.
	ops := kbfsOps.(*KBFSOpsStandard).getOpsByNode(rootNode)
	ops.fbm.forceQuotaReclamation()
	err = ops.fbm.waitForQuotaReclamations(ctx)
	if err != nil {
		t.Fatalf("Couldn't wait for QR: %v", err)
	}

	bserverLocal, ok := config.BlockServer().(*BlockServerLocal)
	if !ok {
		t.Fatalf("Bad block server")
	}
	blocks, err := bserverLocal.getAll(rootNode.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	b := totalBlockRefs(blocks)
	if b <= 1 {
		t.Errorf("Too many blocks left after first QR: %d", b)
	}

	// Now let it run to completion
	for b > 1 {
		ops.fbm.forceQuotaReclamation()
		err = ops.fbm.waitForQuotaReclamations(ctx)
		if err != nil {
			t.Fatalf("Couldn't wait for QR: %v", err)
		}

		blocks, err := bserverLocal.getAll(rootNode.GetFolderBranch().Tlf)
		if err != nil {
			t.Fatalf("Couldn't get blocks: %v", err)
		}
		oldB := b
		b = totalBlockRefs(blocks)
		if b >= oldB {
			t.Fatalf("Blocks didn't shrink after reclamation: %d vs. %d",
				b, oldB)
		}
	}
}

// Test that deleted blocks are correctly flushed from the user cache.
func TestQuotaReclamationDeletedBlocks(t *testing.T) {
	var u1, u2 libkb.NormalizedUsername = "u1", "u2"
	config1, _, ctx := kbfsOpsInitNoMocks(t, u1, u2)
	defer CheckConfigAndShutdown(t, config1)

	now := time.Now()
	clock := &TestClock{now}
	config1.SetClock(clock)

	// Initialize the MD using a different config
	config2 := ConfigAsUser(config1.(*ConfigLocal), u2)
	defer CheckConfigAndShutdown(t, config2)
	config2.SetClock(clock)

	name := u1.String() + "," + u2.String()
	kbfsOps1 := config1.KBFSOps()
	rootNode1, _, err :=
		kbfsOps1.GetOrCreateRootNode(ctx, name, false, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	data := []byte{1, 2, 3, 4, 5}
	aNode1, _, err := kbfsOps1.CreateFile(ctx, rootNode1, "a", false)
	if err != nil {
		t.Fatalf("Couldn't create dir: %v", err)
	}
	err = kbfsOps1.Write(ctx, aNode1, data, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}
	err = kbfsOps1.Sync(ctx, aNode1)
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	// u2 reads the file
	kbfsOps2 := config2.KBFSOps()
	rootNode2, _, err :=
		kbfsOps2.GetOrCreateRootNode(ctx, name, false, MasterBranch)
	if err != nil {
		t.Fatalf("Couldn't create folder: %v", err)
	}
	aNode2, _, err := kbfsOps2.Lookup(ctx, rootNode2, "a")
	if err != nil {
		t.Fatalf("Couldn't create dir: %v", err)
	}
	data2 := make([]byte, len(data))
	_, err = kbfsOps2.Read(ctx, aNode2, data2, 0)
	if err != nil {
		t.Fatalf("Couldn't read file: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("Read bad data: %v", data2)
	}

	// Remove the file
	err = kbfsOps1.RemoveEntry(ctx, rootNode1, "a")
	if err != nil {
		t.Fatalf("Couldn't remove file: %v", err)
	}

	// Wait for outstanding archives
	err = kbfsOps1.SyncFromServer(ctx, rootNode1.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	// Get the current set of blocks
	bserverLocal, ok := config1.BlockServer().(*BlockServerLocal)
	if !ok {
		t.Fatalf("Bad block server")
	}
	preQRBlocks, err := bserverLocal.getAll(rootNode1.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	clock.T = now.Add(2 * config1.QuotaReclamationMinUnrefAge())
	ops1 := kbfsOps1.(*KBFSOpsStandard).getOpsByNode(rootNode1)
	ops1.fbm.forceQuotaReclamation()
	err = ops1.fbm.waitForQuotaReclamations(ctx)
	if err != nil {
		t.Fatalf("Couldn't wait for QR: %v", err)
	}

	postQRBlocks, err := bserverLocal.getAll(rootNode1.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	if pre, post := totalBlockRefs(preQRBlocks),
		totalBlockRefs(postQRBlocks); post >= pre {
		t.Errorf("Blocks didn't shrink after reclamation: pre: %d, post %d",
			pre, post)
	}

	// Sync u2
	err = kbfsOps2.SyncFromServer(ctx, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	// Make the same file on node 2, making sure this doesn't try to
	// reuse the same block (i.e., there are only 2 put calls).
	bNode, _, err := kbfsOps2.CreateFile(ctx, rootNode2, "b", false)
	if err != nil {
		t.Fatalf("Couldn't create dir: %v", err)
	}
	err = kbfsOps2.Write(ctx, bNode, data, 0)
	if err != nil {
		t.Fatalf("Couldn't write file: %v", err)
	}

	// Stall the puts that comes as part of the sync call.
	onWriteStalledCh := make(chan struct{}, 2)
	writeUnstallCh := make(chan struct{})
	stallKey := "requestName"
	writeValue := "write"
	config2.SetBlockOps(&stallingBlockOps{
		stallOpName: "Put",
		stallKey:    stallKey,
		stallMap: map[interface{}]staller{
			writeValue: staller{
				stalled: onWriteStalledCh,
				unstall: writeUnstallCh,
			},
		},
		delegate: config2.BlockOps(),
	})

	// Start the sync and wait for it to stall twice only.
	errChan := make(chan error)
	go func() {
		syncCtx := context.WithValue(ctx, stallKey, writeValue)
		errChan <- kbfsOps2.Sync(syncCtx, bNode)
	}()
	<-onWriteStalledCh
	<-onWriteStalledCh
	writeUnstallCh <- struct{}{}
	writeUnstallCh <- struct{}{}
	// Don't close the channel, we want to make sure other Puts get
	// stalled.
	err = <-errChan
	if err != nil {
		t.Fatalf("Couldn't sync file: %v", err)
	}

	postBBlocks, err := bserverLocal.getAll(rootNode2.GetFolderBranch().Tlf)
	if err != nil {
		t.Fatalf("Couldn't get blocks: %v", err)
	}

	// Wait for outstanding archives
	err = kbfsOps2.SyncFromServer(ctx, rootNode2.GetFolderBranch())
	if err != nil {
		t.Fatalf("Couldn't sync from server: %v", err)
	}

	// There should be exactly 4 extra blocks (2 for the create, and 2
	// for the write/sync) as a result of the operations, and none
	// should have more than one reference.
	if pre, post := totalBlockRefs(postQRBlocks),
		totalBlockRefs(postBBlocks); post != pre+4 {
		t.Errorf("Different number of blocks than expected: pre: %d, post %d",
			pre, post)
	}
	for id, refs := range postBBlocks {
		if len(refs) > 1 {
			t.Errorf("Block %v unexpectedly had %d references", id, len(refs))
		}
	}

}
