package pool

import (
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	redis "github.com/go-redis/redis/v7"
)

const (
	// PollByRandom selects the slave factory by random index
	PollByRandom = iota + 1
	// PollByWeight selects the slave factory by weight
	PollByWeight
	// PollByRoundRobin selects the slave with round-robin order
	PollByRoundRobin
)

type HAConfig struct {
	Master           string
	Slaves           []string
	Password         string
	ReadonlyPassword string
	Options          *redis.Options
	PollType         int

	AutoEjectHost      bool
	ServerFailureLimit int32
	ServerRetryTimeout time.Duration

	weights []int64
}

// HAConnFactory impls the read/write splits between master and slaves
type HAConnFactory struct {
	cfg    *HAConfig
	master *redis.Client
	slaves *clientPool
}

type client struct {
	redisCli *redis.Client

	evicted       bool
	failureCount  int32
	weight        int64
	lastEjectTime int64
}

type clientPool struct {
	pollType           int
	autoEjectHost      bool
	serverFailureLimit int32
	serverRetryTimeout time.Duration

	alives       []*client
	slaves       []*client
	weightRanges []int64

	ind    int
	rand   *rand.Rand
	stopCh chan struct{}
}

func NewHAConnFactory(cfg *HAConfig) (*HAConnFactory, error) {
	if cfg == nil {
		return nil, errors.New("factory cfg shouldn't be empty")
	}
	if err := cfg.init(); err != nil {
		return nil, err
	}

	factory := new(HAConnFactory)
	factory.cfg = cfg
	options := cfg.Options
	options.Addr = cfg.Master
	options.Password = cfg.Password
	factory.master = redis.NewClient(options)
	factory.slaves = newClientPool(cfg)
	return factory, nil
}

func (factory *HAConnFactory) close() {
	factory.master.Close()
	factory.slaves.close()
}

// GetSlaveConnByKey get slave connection
func (factory *HAConnFactory) getSlaveConn(key ...string) (*redis.Client, error) {
	return factory.slaves.getConn(key...)
}

// GetMasterConnByKey get master connection
func (factory *HAConnFactory) getMasterConn(key ...string) (*redis.Client, error) {
	return factory.master, nil
}

func (cfg *HAConfig) init() error {
	var err error

	if cfg.PollType < PollByRandom || cfg.PollType > PollByRoundRobin {
		cfg.PollType = PollByRoundRobin
	}
	if cfg.Options == nil {
		cfg.Options = &redis.Options{}
	}
	cfg.weights = make([]int64, len(cfg.Slaves))
	for i, slave := range cfg.Slaves {
		elems := strings.Split(slave, ":")
		cfg.weights[i] = 100
		if len(elems) == 3 {
			cfg.weights[i], err = strconv.ParseInt(elems[2], 10, 64)
			if err != nil {
				return errors.New("the weight should be integer")
			}
		}
	}
	if cfg.ServerRetryTimeout <= 0 {
		cfg.ServerRetryTimeout = 5 * time.Second
	}
	if cfg.ServerRetryTimeout < 100*time.Millisecond {
		cfg.ServerRetryTimeout = 100 * time.Millisecond
	}
	if cfg.ServerFailureLimit <= 0 {
		cfg.ServerFailureLimit = 3
	}
	return nil
}

func newClient(redisCli *redis.Client, weight int64) *client {
	c := &client{
		redisCli: redisCli,
		weight:   weight,

		failureCount:  0,
		lastEjectTime: 0,
	}
	redisCli.AddHook(newFailureHook(c))
	return c
}

func (c *client) onFailure() {
	atomic.AddInt32(&c.failureCount, 1)
}

func (c *client) onSuccess() {
	atomic.StoreInt32(&c.failureCount, 0)
}

// NewHAConnFactory create new ha factory
func newClientPool(cfg *HAConfig) *clientPool {
	slavePassword := cfg.Password
	if cfg.ReadonlyPassword != "" {
		slavePassword = cfg.ReadonlyPassword
	}
	if len(cfg.Slaves) == 0 {
		cfg.Slaves = append(cfg.Slaves, cfg.Master)
	}

	pool := &clientPool{
		pollType:           cfg.PollType,
		autoEjectHost:      cfg.AutoEjectHost,
		serverFailureLimit: cfg.ServerFailureLimit,
		serverRetryTimeout: cfg.ServerRetryTimeout,

		stopCh: make(chan struct{}, 0),
		rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	pool.slaves = make([]*client, len(cfg.Slaves))
	options := cfg.Options
	for i, slave := range cfg.Slaves {
		slaveOptions := *options
		slaveOptions.Addr = slave
		slaveOptions.Password = slavePassword
		redisCli := redis.NewClient(&slaveOptions)
		pool.slaves[i] = newClient(redisCli, cfg.weights[i])
	}
	pool.alives = pool.slaves
	if pool.pollType == PollByWeight {
		weightRanges := make([]int64, len(pool.alives))
		weightRanges[0] = pool.alives[0].weight
		for i := 1; i < len(pool.alives); i++ {
			weightRanges[i] = weightRanges[i-1] + pool.alives[i].weight
		}
		pool.weightRanges = weightRanges
	}
	go pool.detectFailureTick()
	return pool
}

func (pool *clientPool) getConn(key ...string) (*redis.Client, error) {
	n := len(pool.alives)
	if n == 0 {
		return nil, errors.New("no alive slaves")
	}
	if n == 1 {
		return pool.alives[0].redisCli, nil
	}

	switch pool.pollType {
	case PollByRandom:
		return pool.alives[pool.rand.Intn(n)].redisCli, nil
	case PollByRoundRobin:
		pool.ind = (pool.ind + 1) % n
		return pool.alives[pool.ind].redisCli, nil
	case PollByWeight:
		r := pool.rand.Int63n(pool.weightRanges[n-1])
		for i, weightRange := range pool.weightRanges {
			if r <= weightRange {
				return pool.alives[i].redisCli, nil
			}
		}

		// no reached
		panic("failed to get slave conn")
	default:
		return nil, errors.New("unsupported distribution type")
	}
}

func (p *clientPool) rebuild() {
	if !p.autoEjectHost {
		return
	}
	newAlives := make([]*client, 0)
	for i, slave := range p.slaves {
		if slave.evicted {
			continue
		}
		if slave.failureCount >= p.serverFailureLimit {
			p.slaves[i].lastEjectTime = time.Now().UnixNano()
			p.slaves[i].evicted = true
			continue
		}
		newAlives = append(newAlives, slave)
	}
	if p.alivesEqual(newAlives) {
		return
	}

	if p.pollType == PollByWeight {
		weightRanges := make([]int64, len(newAlives))
		if len(newAlives) >= 1 {
			weightRanges[0] = newAlives[0].weight
			for i := 1; i < len(newAlives); i++ {
				weightRanges[i] = weightRanges[i-1] + newAlives[i].weight
			}
		}
		p.weightRanges = weightRanges
	}
	p.alives = newAlives
}

func (p *clientPool) alivesEqual(newAlives []*client) bool {
	if len(p.alives) != len(newAlives) {
		return false
	}
	for i, alive := range newAlives {
		if alive != p.alives[i] {
			return false
		}
	}
	return true
}

func (p *clientPool) detectFailureTick() {
	interval := time.Second
	if p.serverRetryTimeout < time.Second {
		interval = p.serverRetryTimeout / 2
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if p.autoEjectHost && len(p.slaves) > 1 {
				now := time.Now().UnixNano()
				for i, slave := range p.slaves {
					elapsed := time.Duration(now-slave.lastEjectTime) / time.Millisecond
					if slave.evicted &&
						elapsed >= p.serverRetryTimeout/time.Millisecond &&
						slave.failureCount >= p.serverFailureLimit {
						// noly allow to retry once after evicted
						p.slaves[i].failureCount = p.serverFailureLimit - 1
						p.slaves[i].evicted = false
					}
				}
				p.rebuild()
			}
		}
	}
}

func (p *clientPool) close() {
	for _, slave := range p.slaves {
		slave.redisCli.Close()
	}
}
