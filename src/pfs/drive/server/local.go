package server

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pfs/drive"
	"go.pedge.io/google-protobuf"
	"go.pedge.io/proto/rpclog"
	"go.pedge.io/proto/stream"
	"go.pedge.io/proto/time"
	"golang.org/x/net/context"
)

type localAPIServer struct {
	protorpclog.Logger
	dir string
}

func newLocalAPIServer(dir string) (*localAPIServer, error) {
	server := &localAPIServer{
		Logger: protorpclog.NewLogger("pachyderm.pfs.drive.localAPIServer"),
		dir:    dir,
	}
	if err := os.MkdirAll(server.tmpDir(), 0777); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(server.diffDir(), 0777); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(server.blockDir(), 0777); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *localAPIServer) putOneBlock(scanner *bufio.Scanner) (result *drive.BlockRef, retErr error) {
	hash := newHash()
	tmp, err := ioutil.TempFile(s.tmpDir(), "block")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := tmp.Close(); err != nil && retErr == nil {
			retErr = err
			return
		}
		if result == nil {
			return
		}
		// check if it's a new block
		if _, err := os.Stat(s.blockPath(result.Block)); !os.IsNotExist(err) {
			// already have this block, remove tmp
			if err := os.Remove(tmp.Name()); err != nil && retErr == nil {
				retErr = err
				return
			}
			return
		}
		// it's a new block, rename it accordingly
		if err := os.Rename(tmp.Name(), s.blockPath(result.Block)); err != nil && retErr == nil {
			retErr = err
			return
		}
	}()
	var bytesWritten int
	for scanner.Scan() {
		// they take out the newline, put it back
		bytes := append(scanner.Bytes(), '\n')
		if _, err := hash.Write(bytes); err != nil {
			return nil, err
		}
		if _, err := tmp.Write(bytes); err != nil {
			return nil, err
		}
		bytesWritten += len(bytes)
		if bytesWritten > blockSize {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &drive.BlockRef{
		Block: getBlock(hash),
		Range: &drive.ByteRange{
			Lower: 0,
			Upper: uint64(bytesWritten),
		},
	}, nil
}

func (s *localAPIServer) PutBlock(putBlockServer drive.API_PutBlockServer) (retErr error) {
	result := &drive.BlockRefs{}
	defer func(start time.Time) { s.Log(nil, result, retErr, time.Since(start)) }(time.Now())
	scanner := bufio.NewScanner(protostream.NewStreamingBytesReader(putBlockServer))
	for {
		blockRef, err := s.putOneBlock(scanner)
		if err != nil {
			return err
		}
		result.BlockRef = append(result.BlockRef, blockRef)
		if (blockRef.Range.Upper - blockRef.Range.Lower) < uint64(blockSize) {
			break
		}
	}
	return putBlockServer.SendAndClose(result)
}

func (s *localAPIServer) GetBlock(request *drive.GetBlockRequest, getBlockServer drive.API_GetBlockServer) (retErr error) {
	defer func(start time.Time) { s.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	file, err := os.Open(s.blockPath(request.Block))
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	reader := io.NewSectionReader(file, int64(request.OffsetBytes), int64(request.SizeBytes))
	return protostream.WriteToStreamingBytesServer(reader, getBlockServer)
}

func (s *localAPIServer) InspectBlock(ctx context.Context, request *drive.InspectBlockRequest) (response *drive.BlockInfo, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	stat, err := os.Stat(s.blockPath(request.Block))
	if err != nil {
		return nil, err
	}
	return &drive.BlockInfo{
		Block: request.Block,
		Created: prototime.TimeToTimestamp(
			stat.ModTime(),
		),
		SizeBytes: uint64(stat.Size()),
	}, nil
}

func (s *localAPIServer) ListBlock(ctx context.Context, request *drive.ListBlockRequest) (response *drive.BlockInfos, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return nil, fmt.Errorf("not implemented")
}

func (s *localAPIServer) CreateDiff(ctx context.Context, request *drive.DiffInfo) (response *google_protobuf.Empty, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	data, err := proto.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path.Dir(s.diffPath(request.Diff)), 0777); err != nil {
		return nil, err
	}
	if err := ioutil.WriteFile(s.diffPath(request.Diff), data, 0666); err != nil {
		return nil, err
	}
	return google_protobuf.EmptyInstance, nil
}

func (s *localAPIServer) InspectDiff(ctx context.Context, request *drive.InspectDiffRequest) (response *drive.DiffInfo, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return s.readDiff(request.Diff)
}

func (s *localAPIServer) ListDiff(request *drive.ListDiffRequest, listDiffServer drive.API_ListDiffServer) (retErr error) {
	defer func(start time.Time) { s.Log(request, nil, retErr, time.Since(start)) }(time.Now())
	if err := filepath.Walk(s.diffDir(), func(path string, info os.FileInfo, err error) error {
		diff := s.pathToDiff(path)
		if diff == nil {
			// likely a directory
			return nil
		}
		if diff.Shard == request.Shard {
			diffInfo, err := s.readDiff(diff)
			if err != nil {
				return err
			}
			if err := listDiffServer.Send(diffInfo); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (s *localAPIServer) DeleteDiff(ctx context.Context, request *drive.DeleteDiffRequest) (response *google_protobuf.Empty, retErr error) {
	defer func(start time.Time) { s.Log(request, response, retErr, time.Since(start)) }(time.Now())
	return google_protobuf.EmptyInstance, os.Remove(s.diffPath(request.Diff))
}

func (s *localAPIServer) tmpDir() string {
	return filepath.Join(s.dir, "tmp")
}

func (s *localAPIServer) blockDir() string {
	return filepath.Join(s.dir, "block")
}

func (s *localAPIServer) blockPath(block *drive.Block) string {
	return filepath.Join(s.blockDir(), block.Hash)
}

func (s *localAPIServer) diffDir() string {
	return filepath.Join(s.dir, "diff")
}

func (s *localAPIServer) diffPath(diff *drive.Diff) string {
	return filepath.Join(s.diffDir(), diff.Commit.Repo.Name, diff.Commit.Id, strconv.FormatUint(diff.Shard, 10))
}

// pathToDiff parses a path as a diff, it returns nil when parse fails
func (s *localAPIServer) pathToDiff(path string) *drive.Diff {
	repoCommitShard := strings.Split(strings.TrimPrefix(path, s.diffDir()), "/")
	if len(repoCommitShard) < 3 {
		return nil
	}
	shard, err := strconv.ParseUint(repoCommitShard[2], 10, 64)
	if err != nil {
		return nil
	}
	return &drive.Diff{
		Commit: &pfs.Commit{
			Repo: &pfs.Repo{Name: repoCommitShard[0]},
			Id:   repoCommitShard[1],
		},
		Shard: shard,
	}
}

func (s *localAPIServer) readDiff(diff *drive.Diff) (*drive.DiffInfo, error) {
	data, err := ioutil.ReadFile(s.diffPath(diff))
	if err != nil {
		return nil, err
	}
	result := &drive.DiffInfo{}
	if err := proto.Unmarshal(data, result); err != nil {
		return nil, err
	}
	return result, nil
}
