package route

import (
	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pfs/drive"
	"github.com/pachyderm/pachyderm/src/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/pkg/shard"
	"google.golang.org/grpc"
)

type Sharder interface {
	FileModulus() uint64
	BlockModulus() uint64
	GetShard(file *pfs.File) uint64
	GetBlockShard(block *drive.Block) uint64
}

func NewSharder(fileModulus uint64, blockModulus uint64) Sharder {
	return newSharder(fileModulus, blockModulus)
}

type Router interface {
	GetMasterShards(version int64) (map[uint64]bool, error)
	GetReplicaShards(version int64) (map[uint64]bool, error)
	GetAllShards(version int64) (map[uint64]bool, error)
	GetMasterClientConn(shard uint64, version int64) (*grpc.ClientConn, error)
	GetMasterOrReplicaClientConn(shard uint64, version int64) (*grpc.ClientConn, error)
	GetReplicaClientConns(shard uint64, version int64) ([]*grpc.ClientConn, error)
	GetAllClientConns(version int64) ([]*grpc.ClientConn, error)
}

func NewRouter(
	sharder shard.Sharder,
	dialer grpcutil.Dialer,
	localAddress string,
) Router {
	return newRouter(
		sharder,
		dialer,
		localAddress,
	)
}
