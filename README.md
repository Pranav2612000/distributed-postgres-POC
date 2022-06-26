# distributed-postgres-POC

To be absurdly reductionist, CockroachDB is just an assimilation of a PostgreSQL wire protocol implementation, a storage layer, a Raft implementation for distributed consensus and Postgres's  grammar definition. Using libraries for these I'll build a distributed Postgres POC  ( https://notes.eatonphil.com/distributed-postgres.html )







### Testing

In a terminal start one instance of the db

```bash
./main --node-id node1 --raft-port 2222 --http-port 8222 --pg-port 6000

```



In another terminal start another instance

```bash
./main --node-id node2 --raft-port 2223 --http-port 8223 --pg-port 6001

```



Make node2 follow node1

```bash
curl 'localhost:8222/add-follower?addr=localhost:2223&id=node2'
```



Open psql against port 6000, the leader

```bash
$ psql -h localhost -p 6000
psql -h 127.0.0.1 -p 6000
psql (13.4, server 0.0.0)
Type "help" for help.

phil=> create table x (age int, name text);
CREATE ok
phil=> insert into x values(14, 'garry'), (20, 'ted');
could not interpret result from server: INSERT ok
INSERT ok
phil=> select name, age from x;
  name   | age 
---------+-----
 "garry" |  14
 "ted"   |  20
(2 rows)
```



Now kill `node1` in terminal 1 and start it up again. `node2` will now be the leader. So exit `psql` in terminal 3 and enter it again pointed at `node2`, port `6001`. Add new data.



```bash
$ psql -h 127.0.0.1 -p 6001
psql (13.4, server 0.0.0)
Type "help" for help.

phil=> insert into x values(19, 'ava'), (18, 'ming');
could not interpret result from server: INSERT ok
phil=> select age, name from x;
 age |  name
-----+---------
  20 | "ted"
  14 | "garry"
  18 | "ming"
  19 | "ava"
```



Exit `psql` in terminal 3 and start it up again against `node1` again, port `6000`.

```bash
$ psql -h 127.0.0.1 -p 6000
psql (13.4, server 0.0.0)
Type "help" for help.

phil=> select age, name from x;
 age |  name
-----+---------
  20 | "ted"
  14 | "garry"
  18 | "ming"
  19 | "ava"
(2 rows)
```



Data added to node2 should be accessible through node1
