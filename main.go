package main

import (
  "bytes"
  "encoding/json"
  "fmt"
  "io"
  "log"
  "net"
  "net/http"
  "os"
  "path"
  "strings"
  "time"

  "github.com/google/uuid"
  "github.com/hashicorp/raft"
  "github.com/hashicorp/raft-boltdb"
  "github.com/jackc/pgproto3/v2"
  pgquery "github.com/pganalyze/pg_query_go/v2"
  bolt "go.etcd.io/bbolt"
)

type pgEngine struct {
  db *bolt.DB
  bucketName []byte
}

type pgFsm struct {
  pe *pgEngine
}

type snapshotNoop struct{}

type httpServer struct {
  r *raft.Raft
}

func newPgEngine(db *bolt.DB) *pgEngine {
  return &pgEngine{db, []byte("data")}
}

func (pe *pgEngine) execute(tree *pgquery.ParseResult) error {
  for _, stmt := range tree.GetStmts() {
    n := stmt.GetStmt();
    if c := n.GetCreateStmt(); c != nil {
      return pe.executeCreate(c)
    }

    if c := n.GetInsertStmt(); c != nil {
      return pe.executeInsert(c)
    }

    if c := n.GetSelectStmt(); c != nil {
      _, err := pe.executeSelect(c)
      return err
    }

    return fmt.Errorf("Unknown statement type: %s", stmt)
  }

  return nil
}

func (sn snapshotNoop) Persist(sink raft.SnapshotSink) error {
  return sink.Cancel()
}

func (sn snapshotNoop) Release() {}

func (pf *pgFsm) Snapshot() (raft.FSMSnapshot, error) {
  return snapshotNoop{}, nil
}

func (pf *pgFsm) Restore(rc io.ReadCloser) error {
  return fmt.Errorf("Nothing to restore")
}

func (pf *pgFsm) Apply(log *raft.Log) interface{} {
  switch log.Type {
  case raft.LogCommand:
    ast, err := pgquery.Parse(string(log.Data));
    if err != nil {
      panic(fmt.Errorf("Could not parse payload: %s", err))
    }

    err = pf.pe.execute(ast)
    if err != nil {
      panic(err)
    }
  default:
    panic(fmt.Errorf("Unknown raft log type: %#v", log.Type))
  }

  return nil
}

func setupRaft(dir, nodeId, raftAddress string, pf *pgFsm) (*raft.Raft, error) {
  os.Mkdir(dir, os.ModePerm)
  store, err := raftboltdb.NewBoltStore(path.Join(dir, "bolt"))
  if err != nil {
    return nil, fmt.Errorf("Could not create bolt store: %s", err)
  }

  snapshots, err := raft.NewFileSnapshotStore(path.Join(dir, "snapshot"), 2, os.Stderr)
  if err != nil {
    return nil, fmt.Errorf("Could not create snapshot store: %s", err)
  }

  tcpAddr, err := net.ResolveTCPAddr("tcp", raftAddress)
  if err != nil {
    return nil, fmt.Errorf("Could not resolve address: %s", err)
  }

  transport, err := raft.NewTCPTransport(raftAddress, tcpAddr, 10, time.Second * 10, os.Stderr)
  if err != nil {
    return nil, fmt.Errorf("Could not create tcp transport: %s", err)
  }

  raftCfg := raft.DefaultConfig()
  raftCfg.LocalID = raft.ServerID(nodeId)

  r, err := raft.NewRaft(raftCfg, pf, store, store, snapshots, transport)
  if err != nil {
    return nil, fmt.Errorf("Could not create raft instance: %s", err)
  }

  r.BootstrapCluster(raft.Configuration{
    Servers: []raft.Server{
      {
        ID: raft.ServerID(nodeId),
        Address: transport.LocalAddr(),
      },
    },
  })

  return r, nil
}

func (hs httpServer) addFollowerHandler(w http.ResponseWriter, r *http.Request) {
  followerId := r.URL.Query().Get("id")
  followerAddr := r.URL.Query().Get("addr")

  if hs.r.State() != raft.Leader {
    json.NewEncoder(w).Encode(struct {
      Error string `json:"error"`
    }{
      "Not the leader",
    })
    http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
    return
  }

  err := hs.r.AddVoter(raft.ServerID(followerId), raft.ServerAddress(followerAddr), 0, 0).Error()

  if err != nil {
    log.Printf("Failed to add follower: %s", err)
    http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
    return
  }

  w.WriteHeader(http.StatusOk)
}

/*























*/