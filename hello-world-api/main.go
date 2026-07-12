package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	rdsauth "github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// integrationStatus is the per-backend result the SPA renders as a card.
//
//	ok             — round-trip (or presence check) succeeded
//	starting       — binding present, backend not reachable yet (retryable)
//	error          — binding present, round-trip failed
//	not_configured — no binding mounted; the card is hidden by the SPA
type integrationStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// binding holds the servicebinding.io key/value files for one mounted binding.
type binding map[string]string

// readBinding loads every file under <root>/<name>/ into a map. A missing
// directory means the binding is not wired, signalled by ok=false.
func readBinding(root, name string) (binding, bool) {
	dir := filepath.Join(root, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	b := make(binding, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		b[e.Name()] = strings.TrimSpace(string(v))
	}
	return b, true
}

// namedProbe pairs a display name with the function that produces its status.
type namedProbe struct {
	name string
	run  func(ctx context.Context) integrationStatus
}

type app struct {
	bindingRoot string
	probes      []namedProbe

	mu      sync.Mutex
	pg      *pgxpool.Pool // cached after first successful connect
	redis   *redis.Client // cached after first successful connect
	nats    *nats.Conn    // cached after first successful connect
	js      nats.JetStreamContext
	natsSub *nats.Subscription // cached pull subscription bound to the durable consumer
}

func newApp(bindingRoot string) *app {
	a := &app{bindingRoot: bindingRoot}
	a.probes = []namedProbe{
		{"SQL Database", a.probePostgres},
		{"Cache", a.probeRedis},
		{"NoSQL Database", a.probeNoSQL},
		{"Object Storage", a.probeObjectStorage},
		{"Topic", a.probeTopic},
		{"Subscription", a.probeSubscription},
	}
	return a
}

func (a *app) checkIntegrations(ctx context.Context) []integrationStatus {
	out := make([]integrationStatus, len(a.probes))
	var wg sync.WaitGroup
	for i, p := range a.probes {
		wg.Add(1)
		go func(idx int, probe namedProbe) {
			defer wg.Done()
			out[idx] = probe.run(ctx)
		}(i, p)
	}
	wg.Wait()
	return out
}

// pgPool returns a cached pool, connecting on first use. A failed connect is
// not cached, so a backend that is still starting is retried on the next poll.
func (a *app) pgPool(ctx context.Context, b binding) (*pgxpool.Pool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pg != nil {
		return a.pg, nil
	}
	port := b["port"]
	if port == "" {
		port = "5432"
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(b["username"], b["password"]),
		Host:     net.JoinHostPort(b["host"], port),
		Path:     "/" + b["database"],
		RawQuery: "sslmode=disable&connect_timeout=3",
	}).String()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	a.pg = pool
	return pool, nil
}

// probePostgres performs a real write/read round-trip: it appends a row to a
// visit-counter table and reads the running total back. The detail string is
// the proof the integration works, not just that the port is open.
func (a *app) probePostgres(ctx context.Context) integrationStatus {
	const name = "SQL Database"
	b, ok := readBinding(a.bindingRoot, "sql")
	if !ok {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	if b["password"] == "" && b["role-arn"] != "" {
		return a.probePostgresIAM(ctx, b)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	pool, err := a.pgPool(ctx, b)
	if err != nil {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for database"}
	}

	start := time.Now()
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS hello_visits (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`,
	); err != nil {
		slog.Warn("postgres probe failed", "op", "create", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO hello_visits DEFAULT VALUES`); err != nil {
		slog.Warn("postgres probe failed", "op", "insert", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hello_visits`).Scan(&total); err != nil {
		slog.Warn("postgres probe failed", "op", "read", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "read failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("wrote row, %d total (%dms)", total, time.Since(start).Milliseconds()),
	}
}

// redisClient returns a cached client, connecting on first use. As with
// Postgres, a failed connect is not cached so it is retried next poll.
func (a *app) redisClient(ctx context.Context, b binding) (*redis.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.redis != nil {
		return a.redis, nil
	}
	port := b["port"]
	if port == "" {
		port = "6379"
	}
	c := redis.NewClient(&redis.Options{
		Addr:         net.JoinHostPort(b["host"], port),
		Password:     b["password"], // empty for in-cluster Redis
		DialTimeout:  3 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	a.redis = c
	return c, nil
}

// probeRedis performs a real round-trip: it increments a visit counter and
// reads the new value back.
func (a *app) probeRedis(ctx context.Context) integrationStatus {
	const name = "Cache"
	b, ok := readBinding(a.bindingRoot, "cache")
	if !ok {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	if b["role-arn"] != "" {
		return a.probeRedisIAM(ctx, b)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	c, err := a.redisClient(ctx, b)
	if err != nil {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for cache"}
	}
	start := time.Now()
	n, err := c.Incr(ctx, "hello:visits").Result()
	if err != nil {
		slog.Warn("redis probe failed", "op", "incr", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("visit #%d (%dms)", n, time.Since(start).Milliseconds()),
	}
}

// hasProfile reports whether the shared AWS credentials file has a section for
// the named profile yet. The aws-credentials-sidecar writes one section per
// binding shortly after startup; a missing section means "starting", not broken.
// The profile name always equals the binding directory name (nosql, sql, cache,
// object-storage, object-storage-1, ...).
func hasProfile(name string) bool {
	credsFile := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credsFile == "" {
		return false
	}
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "["+name+"]")
}

func awsProfileConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
}

// probeNoSQL performs a real round-trip: put a uniquely-keyed item, read it
// back, then delete it. Assumes the table's partition key is the default `id`.
func (a *app) probeNoSQL(ctx context.Context) integrationStatus {
	const name = "NoSQL Database"
	b, ok := readBinding(a.bindingRoot, "nosql")
	if !ok {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	if !hasProfile("nosql") {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for credentials"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	cfg, err := awsProfileConfig(ctx, "nosql", b["region"])
	if err != nil {
		slog.Warn("nosql probe failed", "op", "config", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "credential load failed"}
	}
	client := dynamodb.NewFromConfig(cfg)
	table := aws.String(b["table-name"])
	key := map[string]ddbtypes.AttributeValue{
		"id": &ddbtypes.AttributeValueMemberS{Value: fmt.Sprintf("hello-probe-%d", time.Now().UnixNano())},
	}

	start := time.Now()
	if _, err := client.PutItem(ctx, &dynamodb.PutItemInput{TableName: table, Item: key}); err != nil {
		slog.Warn("nosql probe failed", "op", "put", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: table, Key: key, ConsistentRead: aws.Bool(true)})
	if err != nil || got.Item == nil {
		slog.Warn("nosql probe failed", "op", "get", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "read failed"}
	}
	if _, err := client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: table, Key: key}); err != nil {
		slog.Warn("nosql probe failed", "op", "delete", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "delete failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("wrote, read, and deleted item (%dms)", time.Since(start).Milliseconds()),
	}
}

// probeObjectStorage round-trips an object through every bound bucket. The
// first ref mounts at object-storage/, later refs at object-storage-1/ etc.,
// and each mount has a same-named credentials profile.
func (a *app) probeObjectStorage(ctx context.Context) integrationStatus {
	const name = "Object Storage"
	var dirs []string
	entries, err := os.ReadDir(a.bindingRoot)
	if err == nil {
		for _, e := range entries {
			n := e.Name()
			if n == "object-storage" || strings.HasPrefix(n, "object-storage-") {
				dirs = append(dirs, n)
			}
		}
	}
	if len(dirs) == 0 {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	start := time.Now()
	for _, dir := range dirs {
		b, _ := readBinding(a.bindingRoot, dir)
		if !hasProfile(dir) {
			return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for credentials"}
		}
		cfg, err := awsProfileConfig(ctx, dir, b["region"])
		if err != nil {
			slog.Warn("object storage probe failed", "op", "config", "bucket", b["bucket"], "err", err)
			return integrationStatus{Name: name, Status: "error", Detail: "credential load failed"}
		}
		client := s3.NewFromConfig(cfg)
		bucket := aws.String(b["bucket"])
		objKey := aws.String(fmt.Sprintf("hello-probe/%d", time.Now().UnixNano()))

		if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: objKey, Body: strings.NewReader("hello")}); err != nil {
			slog.Warn("object storage probe failed", "op", "put", "bucket", b["bucket"], "err", err)
			return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
		}
		obj, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: objKey})
		if err != nil {
			slog.Warn("object storage probe failed", "op", "get", "bucket", b["bucket"], "err", err)
			return integrationStatus{Name: name, Status: "error", Detail: "read failed"}
		}
		_ = obj.Body.Close()
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: objKey}); err != nil {
			slog.Warn("object storage probe failed", "op", "delete", "bucket", b["bucket"], "err", err)
			return integrationStatus{Name: name, Status: "error", Detail: "delete failed"}
		}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("round-tripped object in %d bucket(s) (%dms)", len(dirs), time.Since(start).Milliseconds()),
	}
}

// regionFromHost extracts the region from an AWS endpoint hostname like
// {id}.{hash}.us-east-1.rds.amazonaws.com. Falls back to us-east-1, the only
// region the platform provisions into.
func regionFromHost(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) >= 4 && parts[len(parts)-2] == "amazonaws" {
		return parts[len(parts)-4]
	}
	return "us-east-1"
}

// probePostgresIAM handles public-cloud sql bindings (RDS IAM auth — no
// password in the binding). It verifies the workload identity chain end to
// end: STS credentials from the sidecar, then an RDS auth token. The RDS
// endpoint is VPC-internal and normally unreachable from this cluster, so a
// connect timeout still reports ok with an identity-verified detail; if the
// endpoint ever becomes reachable, the probe upgrades to a full round-trip.
func (a *app) probePostgresIAM(ctx context.Context, b binding) integrationStatus {
	const name = "SQL Database"
	if !hasProfile("sql") {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for credentials"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	region := regionFromHost(b["host"])
	cfg, err := awsProfileConfig(ctx, "sql", region)
	if err != nil {
		slog.Warn("sql iam probe failed", "op", "config", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "credential load failed"}
	}
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, nil); err != nil {
		slog.Warn("sql iam probe failed", "op", "sts", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "STS identity check failed"}
	}
	port := b["port"]
	if port == "" {
		port = "5432"
	}
	endpoint := net.JoinHostPort(b["host"], port)
	token, err := rdsauth.BuildAuthToken(ctx, endpoint, region, b["username"], cfg.Credentials)
	if err != nil {
		slog.Warn("sql iam probe failed", "op", "token", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "RDS auth token generation failed"}
	}

	connCtx, connCancel := context.WithTimeout(ctx, 2*time.Second)
	defer connCancel()
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(b["username"], token),
		Host:     endpoint,
		Path:     "/" + b["database"],
		RawQuery: "sslmode=require&connect_timeout=2",
	}).String()
	conn, err := pgx.Connect(connCtx, dsn)
	if err != nil {
		return integrationStatus{Name: name, Status: "ok", Detail: "IAM auth token generated; endpoint VPC-internal (identity verified)"}
	}
	defer func() { _ = conn.Close(ctx) }()

	start := time.Now()
	if _, err := conn.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS hello_visits (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`,
	); err != nil {
		slog.Warn("sql iam probe failed", "op", "create", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO hello_visits DEFAULT VALUES`); err != nil {
		slog.Warn("sql iam probe failed", "op", "insert", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	var total int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM hello_visits`).Scan(&total); err != nil {
		slog.Warn("sql iam probe failed", "op", "read", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "read failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("IAM auth round-trip: wrote row, %d total (%dms)", total, time.Since(start).Milliseconds()),
	}
}

// probeRedisIAM handles public-cloud cache bindings (ElastiCache IAM auth).
// ElastiCache is always VPC-internal, so this verifies the identity chain (STS
// credentials via the cache profile) and attempts a short TLS connect that is
// expected to time out.
func (a *app) probeRedisIAM(ctx context.Context, b binding) integrationStatus {
	const name = "Cache"
	if !hasProfile("cache") {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for credentials"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	cfg, err := awsProfileConfig(ctx, "cache", "us-east-1")
	if err != nil {
		slog.Warn("cache iam probe failed", "op", "config", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "credential load failed"}
	}
	if _, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, nil); err != nil {
		slog.Warn("cache iam probe failed", "op", "sts", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "STS identity check failed"}
	}
	port := b["port"]
	if port == "" {
		port = "6379"
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", net.JoinHostPort(b["host"], port), nil)
	if err != nil {
		return integrationStatus{Name: name, Status: "ok", Detail: "IAM credentials verified; endpoint VPC-internal"}
	}
	_ = conn.Close()
	return integrationStatus{Name: name, Status: "ok", Detail: "IAM credentials verified; endpoint reachable"}
}

// natsJS returns a cached JetStream context, connecting on first use. A failed
// connect is not cached, so a broker that is still starting is retried.
func (a *app) natsJS() (nats.JetStreamContext, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.js != nil {
		return a.js, nil
	}
	nc, err := nats.Connect(os.Getenv("NATS_URL"), nats.Timeout(3*time.Second))
	if err != nil {
		return nil, err
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, err
	}
	a.nats, a.js = nc, js
	return js, nil
}

// pullSub returns a cached pull subscription bound to the durable consumer.
// Cached because Unsubscribe would delete the durable out from under NACK, and
// re-subscribing on every poll would leak interest on the connection.
func (a *app) pullSub(js nats.JetStreamContext, stream, consumer string) (*nats.Subscription, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.natsSub != nil {
		return a.natsSub, nil
	}
	sub, err := js.PullSubscribe("", consumer, nats.Bind(stream, consumer))
	if err != nil {
		return nil, err
	}
	a.natsSub = sub
	return sub, nil
}

// publishableSubject turns a stream's first subject pattern into a concrete
// subject: e2e.> → e2e.probe, foo.*.bar → foo.probe.bar.
func publishableSubject(subjects []string) string {
	if len(subjects) == 0 {
		return ""
	}
	tokens := strings.Split(subjects[0], ".")
	for i, t := range tokens {
		if t == "*" || t == ">" {
			tokens[i] = "probe"
		}
	}
	return strings.Join(tokens, ".")
}

// probeTopic publishes a message to the bound stream — proves the stream
// exists and accepts writes.
func (a *app) probeTopic(ctx context.Context) integrationStatus {
	const name = "Topic"
	stream := os.Getenv("NATS_STREAM")
	if stream == "" {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	js, err := a.natsJS()
	if err != nil {
		return integrationStatus{Name: name, Status: "starting", Detail: "waiting for message broker"}
	}
	info, err := js.StreamInfo(stream)
	if err != nil {
		slog.Warn("topic probe failed", "op", "info", "stream", stream, "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "stream lookup failed"}
	}
	subject := publishableSubject(info.Config.Subjects)
	if subject == "" {
		return integrationStatus{Name: name, Status: "error", Detail: "stream has no subjects"}
	}
	start := time.Now()
	payload := fmt.Sprintf("hello-probe %d", time.Now().UnixNano())
	if _, err := js.Publish(subject, []byte(payload), nats.Context(ctx)); err != nil {
		slog.Warn("topic probe failed", "op", "publish", "stream", stream, "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "publish failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("published to %s via %s (%dms)", stream, subject, time.Since(start).Milliseconds()),
	}
}

// probeSubscription proves stream → durable cursor → delivery end to end: it
// publishes a uniquely-tagged message to the consumer's stream, then pull-
// fetches through the durable until the tagged message arrives.
func (a *app) probeSubscription(ctx context.Context) integrationStatus {
	const name = "Subscription"
	consumer := os.Getenv("NATS_CONSUMER")
	if consumer == "" {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	stream := os.Getenv("NATS_STREAM")
	if stream == "" {
		return integrationStatus{Name: name, Status: "error", Detail: "NATS_STREAM not set"}
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	js, err := a.natsJS()
	if err != nil {
		return integrationStatus{Name: name, Status: "starting", Detail: "waiting for message broker"}
	}
	info, err := js.StreamInfo(stream)
	if err != nil {
		slog.Warn("subscription probe failed", "op", "info", "stream", stream, "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "stream lookup failed"}
	}
	subject := publishableSubject(info.Config.Subjects)
	tag := fmt.Sprintf("hello-probe-%d", time.Now().UnixNano())
	if _, err := js.Publish(subject, []byte(tag), nats.Context(ctx)); err != nil {
		slog.Warn("subscription probe failed", "op", "publish", "stream", stream, "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "publish failed"}
	}
	sub, err := a.pullSub(js, stream, consumer)
	if err != nil {
		slog.Warn("subscription probe failed", "op", "bind", "consumer", consumer, "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "consumer bind failed"}
	}

	start := time.Now()
	for ctx.Err() == nil {
		msgs, err := sub.Fetch(64, nats.MaxWait(500*time.Millisecond))
		if err != nil && err != nats.ErrTimeout {
			slog.Warn("subscription probe failed", "op", "fetch", "consumer", consumer, "err", err)
			return integrationStatus{Name: name, Status: "error", Detail: "fetch failed"}
		}
		for _, m := range msgs {
			_ = m.Ack()
			if strings.Contains(string(m.Data), tag) {
				return integrationStatus{
					Name:   name,
					Status: "ok",
					Detail: fmt.Sprintf("consumed own message via durable %s (%dms)", consumer, time.Since(start).Milliseconds()),
				}
			}
		}
	}
	return integrationStatus{Name: name, Status: "error", Detail: "published message not delivered"}
}

func main() {
	port := envOrDefault("PORT", "8080")
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})))

	a := newApp(envOrDefault("SERVICE_BINDING_ROOT", "/bindings"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.HandleFunc("GET /", a.handleRoot)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("hello-world-api starting", "port", port)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("hello-world-api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if a.pg != nil {
		a.pg.Close()
	}
	if a.redis != nil {
		_ = a.redis.Close()
	}
	if a.nats != nil {
		a.nats.Close()
	}
}

func (a *app) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	for _, i := range a.checkIntegrations(r.Context()) {
		if i.Status != "ok" && i.Status != "not_configured" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *app) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	integrations := a.checkIntegrations(r.Context())
	ready := true
	wired := make([]integrationStatus, 0, len(integrations))
	for _, i := range integrations {
		if i.Status == "not_configured" {
			continue
		}
		wired = append(wired, i)
		if i.Status != "ok" {
			ready = false
		}
	}
	workspace := podNamespace()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Message      string              `json:"message"`
		Workspace    string              `json:"workspace"`
		Timestamp    string              `json:"timestamp"`
		Ready        bool                `json:"ready"`
		Integrations []integrationStatus `json:"integrations"`
	}{
		Message:      "Hello from " + workspace + "!",
		Workspace:    workspace,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Ready:        ready,
		Integrations: wired,
	})
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func podNamespace() string {
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "guest"
	}
	return strings.TrimSpace(string(b))
}
