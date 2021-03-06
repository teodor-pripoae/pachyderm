/*
Package pfsutil provides utility functions that wrap a pfs.APIClient
to make the calling code slightly cleaner.
*/
package pfsutil

import (
	"io"
	"math"

	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pfs/drive"
	"go.pedge.io/proto/stream"
	"golang.org/x/net/context"
)

const chunkSize = 4096

func NewRepo(repoName string) *pfs.Repo {
	return &pfs.Repo{Name: repoName}
}

func NewCommit(repoName string, commitID string) *pfs.Commit {
	return &pfs.Commit{
		Repo: &pfs.Repo{
			Name: repoName,
		},
		Id: commitID,
	}
}

func NewFile(repoName string, commitID string, path string) *pfs.File {
	return &pfs.File{
		Commit: &pfs.Commit{
			Repo: &pfs.Repo{
				Name: repoName,
			},
			Id: commitID,
		},
		Path: path,
	}
}

func CreateRepo(apiClient pfs.APIClient, repoName string) error {
	_, err := apiClient.CreateRepo(
		context.Background(),
		&pfs.CreateRepoRequest{
			Repo: &pfs.Repo{
				Name: repoName,
			},
		},
	)
	return err
}

func InspectRepo(apiClient pfs.APIClient, repoName string) (*pfs.RepoInfo, error) {
	repoInfo, err := apiClient.InspectRepo(
		context.Background(),
		&pfs.InspectRepoRequest{
			Repo: &pfs.Repo{
				Name: repoName,
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return repoInfo, nil
}

func ListRepo(apiClient pfs.APIClient) ([]*pfs.RepoInfo, error) {
	repoInfos, err := apiClient.ListRepo(
		context.Background(),
		&pfs.ListRepoRequest{},
	)
	if err != nil {
		return nil, err
	}
	return repoInfos.RepoInfo, nil
}

func DeleteRepo(apiClient pfs.APIClient, repoName string) error {
	_, err := apiClient.DeleteRepo(
		context.Background(),
		&pfs.DeleteRepoRequest{
			Repo: &pfs.Repo{
				Name: repoName,
			},
		},
	)
	return err
}

func StartCommit(apiClient pfs.APIClient, repoName string, parentCommit string) (*pfs.Commit, error) {
	commit, err := apiClient.StartCommit(
		context.Background(),
		&pfs.StartCommitRequest{
			Parent: &pfs.Commit{
				Repo: &pfs.Repo{
					Name: repoName,
				},
				Id: parentCommit,
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return commit, nil
}

func FinishCommit(apiClient pfs.APIClient, repoName string, commitID string) error {
	_, err := apiClient.FinishCommit(
		context.Background(),
		&pfs.FinishCommitRequest{
			Commit: &pfs.Commit{
				Repo: &pfs.Repo{
					Name: repoName,
				},
				Id: commitID,
			},
		},
	)
	return err
}

func InspectCommit(apiClient pfs.APIClient, repoName string, commitID string) (*pfs.CommitInfo, error) {
	commitInfo, err := apiClient.InspectCommit(
		context.Background(),
		&pfs.InspectCommitRequest{
			Commit: &pfs.Commit{
				Repo: &pfs.Repo{
					Name: repoName,
				},
				Id: commitID,
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return commitInfo, nil
}

func ListCommit(apiClient pfs.APIClient, repoNames []string) ([]*pfs.CommitInfo, error) {
	var repos []*pfs.Repo
	for _, repoName := range repoNames {
		repos = append(repos, &pfs.Repo{Name: repoName})
	}
	commitInfos, err := apiClient.ListCommit(
		context.Background(),
		&pfs.ListCommitRequest{
			Repo: repos,
		},
	)
	if err != nil {
		return nil, err
	}
	return commitInfos.CommitInfo, nil
}

func DeleteCommit(apiClient pfs.APIClient, repoName string, commitID string) error {
	_, err := apiClient.DeleteCommit(
		context.Background(),
		&pfs.DeleteCommitRequest{
			Commit: &pfs.Commit{
				Repo: &pfs.Repo{
					Name: repoName,
				},
				Id: commitID,
			},
		},
	)
	return err
}

func PutBlock(apiClient drive.APIClient, reader io.Reader) (*drive.BlockRefs, error) {
	putBlockClient, err := apiClient.PutBlock(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(protostream.NewStreamingBytesWriter(putBlockClient), reader); err != nil {
		return nil, err
	}
	return putBlockClient.CloseAndRecv()
}

func GetBlock(apiClient drive.APIClient, hash string, offsetBytes uint64, sizeBytes uint64) (io.Reader, error) {
	apiGetBlockClient, err := apiClient.GetBlock(
		context.Background(),
		&drive.GetBlockRequest{
			Block: &drive.Block{
				Hash: hash,
			},
			OffsetBytes: offsetBytes,
			SizeBytes:   sizeBytes,
		},
	)
	if err != nil {
		return nil, err
	}
	return protostream.NewStreamingBytesReader(apiGetBlockClient), nil
}

func InspectBlock(apiClient drive.APIClient, hash string) (*drive.BlockInfo, error) {
	blockInfo, err := apiClient.InspectBlock(
		context.Background(),
		&drive.InspectBlockRequest{
			Block: &drive.Block{
				Hash: hash,
			},
		},
	)
	if err != nil {
		return nil, err
	}
	return blockInfo, nil
}

func ListBlock(apiClient drive.APIClient) ([]*drive.BlockInfo, error) {
	blockInfos, err := apiClient.ListBlock(
		context.Background(),
		&drive.ListBlockRequest{},
	)
	if err != nil {
		return nil, err
	}
	return blockInfos.BlockInfo, nil
}

func PutFile(apiClient pfs.APIClient, repoName string, commitID string, path string, offset int64, reader io.Reader) (_ int, retErr error) {
	putFileClient, err := apiClient.PutFile(context.Background())
	if err != nil {
		return 0, err
	}
	defer func() {
		if _, err := putFileClient.CloseAndRecv(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	request := pfs.PutFileRequest{
		File: &pfs.File{
			Commit: &pfs.Commit{
				Repo: &pfs.Repo{
					Name: repoName,
				},
				Id: commitID,
			},
			Path: path,
		},
		FileType:    pfs.FileType_FILE_TYPE_REGULAR,
		OffsetBytes: offset,
	}
	var size int
	for {
		value := make([]byte, chunkSize)
		iSize, err := reader.Read(value)
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
		request.Value = value[0:iSize]
		size += iSize
		if err := putFileClient.Send(&request); err != nil {
			return 0, err
		}
	}
	if err != nil && err != io.EOF {
		return 0, err
	}
	return size, err
}

func GetFile(apiClient pfs.APIClient, repoName string, commitID string, path string, offset int64, size int64, shard *pfs.Shard, writer io.Writer) error {
	if size == 0 {
		size = math.MaxInt64
	}
	apiGetFileClient, err := apiClient.GetFile(
		context.Background(),
		&pfs.GetFileRequest{
			File: &pfs.File{
				Commit: &pfs.Commit{
					Repo: &pfs.Repo{
						Name: repoName,
					},
					Id: commitID,
				},
				Path: path,
			},
			Shard:       shard,
			OffsetBytes: offset,
			SizeBytes:   size,
		},
	)
	if err != nil {
		return err
	}
	if err := protostream.WriteFromStreamingBytesClient(apiGetFileClient, writer); err != nil {
		return err
	}
	return nil
}

func InspectFile(apiClient pfs.APIClient, repoName string, commitID string, path string, shard *pfs.Shard) (*pfs.FileInfo, error) {
	fileInfo, err := apiClient.InspectFile(
		context.Background(),
		&pfs.InspectFileRequest{
			File: &pfs.File{
				Commit: &pfs.Commit{
					Repo: &pfs.Repo{
						Name: repoName,
					},
					Id: commitID,
				},
				Path: path,
			},
			Shard: shard,
		},
	)
	if err != nil {
		return nil, err
	}
	return fileInfo, nil
}

func ListFile(apiClient pfs.APIClient, repoName string, commitID string, path string, shard *pfs.Shard) ([]*pfs.FileInfo, error) {
	fileInfos, err := apiClient.ListFile(
		context.Background(),
		&pfs.ListFileRequest{
			File: &pfs.File{
				Commit: &pfs.Commit{
					Repo: &pfs.Repo{
						Name: repoName,
					},
					Id: commitID,
				},
				Path: path,
			},
			Shard: shard,
		},
	)
	if err != nil {
		return nil, err
	}
	return fileInfos.FileInfo, nil
}

func DeleteFile(apiClient pfs.APIClient, repoName string, commitID string, path string) error {
	_, err := apiClient.DeleteFile(
		context.Background(),
		&pfs.DeleteFileRequest{
			File: &pfs.File{
				Commit: &pfs.Commit{
					Repo: &pfs.Repo{
						Name: repoName,
					},
					Id: commitID,
				},
				Path: path,
			},
		},
	)
	return err
}

func MakeDirectory(apiClient pfs.APIClient, repoName string, commitID string, path string) (retErr error) {
	putFileClient, err := apiClient.PutFile(context.Background())
	if err != nil {
		return err
	}
	defer func() {
		if _, err := putFileClient.CloseAndRecv(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	return putFileClient.Send(
		&pfs.PutFileRequest{
			File: &pfs.File{
				Commit: &pfs.Commit{
					Repo: &pfs.Repo{
						Name: repoName,
					},
					Id: commitID,
				},
				Path: path,
			},
			FileType: pfs.FileType_FILE_TYPE_DIR,
		},
	)
}
