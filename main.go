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

type tableDefinition struct {
  Name        string
  ColumnNames []string
  ColumnTypes []string
}

type pgResult struct {
  fieldNames []string
  fieldTypes []string
  rows       [][]any
}

func (pe *pgEngine) getTableDefinition(name string) (*tableDefinition, error) {
  var tbl tableDefinition
  err := pe.db.View(func(tx *bolt.Tx) error {
    bkt := tx.Bucket(pe.bucketName)
    if bkt == nil {
      return fmt.Errorf("Table does not exist")
    }

    valBytes := bkt.Get([]byte("table_" + name))
    err := json.Unmarshal(valBytes, &tbl)
    if err != nil {
      return fmt.Errorf("Could not unmarshal table: %s", err)
    }

    return nil
  })

  return &tbl, err
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

func (pe *pgEngine) executeCreate(stmt *pgquery.CreateStmt) error {
  tbl := tableDefinition{}
  tbl.Name = stmt.Relation.Relname

  for _, c := range stmt.TableElts {
    cd := c.GetColumnDef()

    tbl.ColumnNames = append(tbl.ColumnNames, cd.Colname)

    var columnType string
    for _, n := range cd.TypeName.Names {
      if columnType != "" {
        columnType += "."
      }
      columnType += n.GetString_().Str
    }
    tbl.ColumnTypes = append(tbl.ColumnTypes, columnType)
  }

  tableBytes, err := json.Marshal(tbl)
  if err != nil {
    return fmt.Errorf("Could not marshall table: %s", err)
  }

  err = pe.db.Update(func (tx *bolt.Tx) error {
    bkt, err := tx.CreateBucketIfNotExists(pe.bucketName)
    if err != nil {
      return err
    }

    return bkt.Put([]byte("tables_" + tbl.Name), tableBytes)
  })

  if err != nil {
    return fmt.Errorf("Could not set key-value store: %s", err)
  }

  return nil
}

func (pe *pgEngine) executeInsert(stmt *pgquery.InsertStmt) error {
  tblName := stmt.Relation.Relname

  slct := stmt.GetSelectStmt().GetSelectStmt()
  for _, values := range slct.ValuesLists {
    var rowData []any
    for _, value := range values.GetList().Items {
      if c := value.GetAConst(); c != nil {
        if s := c.Val.GetString_(); s != nil {
          rowData = append(rowData, s.Str)
          continue
        }

        if i := c.Val.GetInteger(); i  != nil {
          rowData = append(rowData, i.Ival)
          continue
        }
      }

      return fmt.Errorf("Unknown value type: %s", value)
    }

    rowBytes, err := json.Marshal(rowData)
    if err != nil {
      return fmt.Errorf("Could not marshal row: %s", err)
    }

    id := uuid.New().String()
    err = pe.db.Update(func(tx *bolt.Tx) error {
      bkt, err := tx.CreateBucketIfNotExists(pe.bucketName)
      if err != nil {
        return err
      }

      return bkt.Put([]byte("rows_" + tblName + "_" + id), rowBytes)
    })
    if err != nil {
      return fmt.Errorf("Could not insert row: %s", err)
    }
  }

  return nil
}

func (pe *pgEngine) executeSelect(stmt *pgquery.SelectStmt) (*pgResult, error) {
  tblName := stmt.FromClause[0].GetRangeVar().Relname
  tbl, err := pe.getTableDefinition(tblName)
  if err != nil {
    return nil, err
  }

  results := &pgResult{}
  for _, c := range stmt.TargetList {
    fieldName := c.GetResTarget().Val.GetColumnRef().Fields[0].GetString_().Str
    results.fieldNames = append(results.fieldNames, fieldName)

    fieldType := ""
    for i, cn := range tbl.ColumnNames {
      if cn == fieldName {
        fieldType = tbl.ColumnTypes[i]
      }
    }

    if fieldType == "" {
      return nil, fmt.Errorf("Unknown field: %s", err)
    }

    results.fieldTypes = append(results.fieldTypes, fieldType)
  }

  prefix := []byte("rows_" + tblName + "_")
  pe.db.View(func(tx *bolt.Tx) error {
    c := tx.Bucket(pe.bucketName).Cursor()

    for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
      var row []any
      err := json.Unmarshal(v, &row)
      if err != nil {
        return fmt.Errorf("Unable to unmarshal rows: %s", err)
      }

      var targetRow []any
      for _, target := range results.fieldNames {
        for i, field := range tbl.ColumnNames {
          if target == field {
            targetRow = append(targetRow, row[i])
          }
        }
      }

      results.rows = append(results.rows, targetRow)
    }
    return nil
  })
  return results, nil
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

  w.WriteHeader(http.StatusOK)
}

/*























*/
