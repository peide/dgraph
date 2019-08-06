/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo"
	"github.com/dgraph-io/dgo/protos/api"
	minio "github.com/minio/minio-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/dgraph-io/dgraph/ee/backup"
	"github.com/dgraph-io/dgraph/testutil"
	"github.com/dgraph-io/dgraph/x"
)

var (
	backupDir  = "./data/backups"
	restoreDir = "./data/restore"
	testDirs   = []string{backupDir, restoreDir}

	mc                *minio.Client
	bucketName        = "dgraph-backup"
	backupDestination = "minio://minio1:9001/dgraph-backup?secure=false"

	alphaContainers = []string{
		"alpha1",
		"alpha2",
		"alpha3",
	}
)

func TestBackupMinio(t *testing.T) {
	conn, err := grpc.Dial(testutil.SockAddr, grpc.WithInsecure())
	require.NoError(t, err)
	dg := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	mc, err = testutil.NewMinioClient()
	require.NoError(t, err)
	require.NoError(t, mc.MakeBucket(bucketName, ""))

	// Add initial data.
	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `movie: string .`}))
	original, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			<_:x1> <movie> "BIRDS MAN OR (THE UNEXPECTED VIRTUE OF IGNORANCE)" .
			<_:x2> <movie> "Spotlight" .
			<_:x3> <movie> "Moonlight" .
			<_:x4> <movie> "THE SHAPE OF WATERLOO" .
			<_:x5> <movie> "BLACK PUNTER" .
		`),
	})
	require.NoError(t, err)
	t.Logf("--- Original uid mapping: %+v\n", original.Uids)

	// Move tablet to group 1 to avoid messes later.
	_, err = http.Get("http://" + testutil.SockAddrZeroHttp + "/moveTablet?tablet=movie&group=1")
	require.NoError(t, err)

	// After the move, we need to pause a bit to give zero a chance to quorum.
	t.Log("Pausing to let zero move tablet...")
	moveOk := false
	for retry := 5; retry > 0; retry-- {
		time.Sleep(3 * time.Second)
		state, err := testutil.GetState()
		require.NoError(t, err)
		if _, ok := state.Groups["1"].Tablets["movie"]; ok {
			moveOk = true
			break
		}
	}
	require.True(t, moveOk)

	// Setup test directories.
	dirSetup()

	// Send backup request.
	_ = runBackup(t, 3, 1)
	restored := runRestore(t, backupDir, "", math.MaxUint64)

	checks := []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "BIRDS MAN OR (THE UNEXPECTED VIRTUE OF IGNORANCE)"},
		{blank: "x2", expected: "Spotlight"},
		{blank: "x3", expected: "Moonlight"},
		{blank: "x4", expected: "THE SHAPE OF WATERLOO"},
		{blank: "x5", expected: "BLACK PUNTER"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Add more data for the incremental backup.
	incr1, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
			<%s> <movie> "Birdman or (The Unexpected Virtue of Ignorance)" .
			<%s> <movie> "The Shape of Waterloo" .
		`, original.Uids["x1"], original.Uids["x4"])),
	})
	t.Logf("%+v", incr1)
	require.NoError(t, err)

	// Perform first incremental backup.
	_ = runBackup(t, 6, 2)
	restored = runRestore(t, backupDir, "", incr1.Context.CommitTs)

	checks = []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "Birdman or (The Unexpected Virtue of Ignorance)"},
		{blank: "x4", expected: "The Shape of Waterloo"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Add more data for a second incremental backup.
	incr2, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
				<%s> <movie> "The Shape of Water" .
				<%s> <movie> "The Black Panther" .
			`, original.Uids["x4"], original.Uids["x5"])),
	})
	require.NoError(t, err)

	// Perform second incremental backup.
	_ = runBackup(t, 9, 3)
	restored = runRestore(t, backupDir, "", incr2.Context.CommitTs)

	checks = []struct {
		blank, expected string
	}{
		{blank: "x4", expected: "The Shape of Water"},
		{blank: "x5", expected: "The Black Panther"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Add more data for a second full backup.
	incr3, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`
				<%s> <movie> "El laberinto del fauno" .
				<%s> <movie> "Black Panther 2" .
			`, original.Uids["x4"], original.Uids["x5"])),
	})
	require.NoError(t, err)

	// Perform second full backup.
	dirs := runBackupInternal(t, true, 12, 4)
	restored = runRestore(t, backupDir, "", incr3.Context.CommitTs)

	// Check all the values were restored to their most recent value.
	checks = []struct {
		blank, expected string
	}{
		{blank: "x1", expected: "Birdman or (The Unexpected Virtue of Ignorance)"},
		{blank: "x2", expected: "Spotlight"},
		{blank: "x3", expected: "Moonlight"},
		{blank: "x4", expected: "El laberinto del fauno"},
		{blank: "x5", expected: "Black Panther 2"},
	}
	for _, check := range checks {
		require.EqualValues(t, check.expected, restored[original.Uids[check.blank]])
	}

	// Remove the full backup dirs and verify restore catches the error.
	require.NoError(t, os.RemoveAll(dirs[0]))
	require.NoError(t, os.RemoveAll(dirs[3]))
	runFailingRestore(t, backupDir, "", incr3.Context.CommitTs)

	// Clean up test directories.
	dirCleanup()
}

func runBackup(t *testing.T, numExpectedFiles, numExpectedDirs int) []string {
	return runBackupInternal(t, false, numExpectedFiles, numExpectedDirs)
}

func runBackupInternal(t *testing.T, forceFull bool, numExpectedFiles,
	numExpectedDirs int) []string {
	forceFullStr := "false"
	if forceFull {
		forceFullStr = "true"
	}

	resp, err := http.PostForm("http://localhost:8180/admin/backup", url.Values{
		"destination": []string{backupDestination},
		"force_full":  []string{forceFullStr},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, testutil.GetError(resp.Body))
	// TODO(martinmr): remove this sleep.
	time.Sleep(time.Second)

	// Verify that the right amount of files and directories were created.
	copyToLocalFs(t)

	files := x.WalkPathFunc(backupDir, func(path string, isdir bool) bool {
		return !isdir && strings.HasSuffix(path, ".backup")
	})
	require.Equal(t, numExpectedFiles, len(files))

	dirs := x.WalkPathFunc(backupDir, func(path string, isdir bool) bool {
		return isdir && strings.HasPrefix(path, "data/backups/dgraph.")
	})
	require.Equal(t, numExpectedDirs, len(dirs))

	manifests := x.WalkPathFunc(backupDir, func(path string, isdir bool) bool {
		return !isdir && strings.Contains(path, "manifest.json")
	})
	require.Equal(t, numExpectedDirs, len(manifests))

	return dirs
}

func runRestore(t *testing.T, backupLocation, lastDir string, commitTs uint64) map[string]string {
	// Recreate the restore directory to make sure there's no previous data when
	// calling restore.
	require.NoError(t, os.RemoveAll(restoreDir))
	require.NoError(t, os.MkdirAll(restoreDir, os.ModePerm))

	t.Logf("--- Restoring from: %q", backupLocation)
	_, err := backup.RunRestore("./data/restore", backupLocation, lastDir)
	require.NoError(t, err)

	restored, err := testutil.GetPValues("./data/restore/p1", "movie", commitTs)
	require.NoError(t, err)
	t.Logf("--- Restored values: %+v\n", restored)

	return restored
}

// runFailingRestore is like runRestore but expects an error during restore.
func runFailingRestore(t *testing.T, backupLocation, lastDir string, commitTs uint64) {
	// Recreate the restore directory to make sure there's no previous data when
	// calling restore.
	require.NoError(t, os.RemoveAll(restoreDir))
	require.NoError(t, os.MkdirAll(restoreDir, os.ModePerm))

	_, err := backup.RunRestore("./data/restore", backupLocation, lastDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected a BackupNum value of 1")
}

func dirSetup() {
	// Clean up data from previous runs.
	dirCleanup()

	for _, dir := range testDirs {
		x.Check(os.MkdirAll(dir, os.ModePerm))
	}
}

func dirCleanup() {
	x.Check(os.RemoveAll("./data"))
}

func copyToLocalFs(t *testing.T) {
	// List all the folders in the bucket.
	lsCh1 := make(chan struct{})
	defer close(lsCh1)
	objectCh1 := mc.ListObjectsV2(bucketName, "", false, lsCh1)
	for object := range objectCh1 {
		require.NoError(t, object.Err)
		dstDir := backupDir + "/" + object.Key
		os.MkdirAll(dstDir, os.ModePerm)

		// Get all the files in that folder and
		lsCh2 := make(chan struct{})
		defer close(lsCh2)
		objectCh2 := mc.ListObjectsV2(bucketName, "", true, lsCh2)
		for object := range objectCh2 {
			require.NoError(t, object.Err)
			dstFile := backupDir + "/" + object.Key
			mc.FGetObject(bucketName, object.Key, dstFile, minio.GetObjectOptions{})
		}
	}
}
