// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package drainer

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/mysql"
	"go.uber.org/zap"

	dsync "github.com/pingcap/tidb-binlog/drainer/sync"
	"github.com/pingcap/tidb-binlog/pkg/filter"
	"github.com/pingcap/tidb-binlog/pkg/flags"
	"github.com/pingcap/tidb-binlog/pkg/security"
	"github.com/pingcap/tidb-binlog/pkg/util"
	"github.com/pingcap/tidb-binlog/pkg/version"
	"github.com/pingcap/tidb-binlog/pkg/zk"
)

const (
	defaultDataDir        = "data.drainer"
	defaultDetectInterval = 10
	defaultEtcdURLs       = "http://127.0.0.1:2379"
	// defaultEtcdTimeout defines the timeout of dialing or sending request to etcd.
	defaultEtcdTimeout     = 5 * time.Second
	defaultSyncedCheckTime = 5 // 5 minute
	defaultKafkaAddrs      = "127.0.0.1:9092"
	defaultKafkaVersion    = "0.8.2.0"
)

var (
	maxBinlogItemCount        int
	defaultBinlogItemCount    = 16 << 12
	supportedCompressors      = [...]string{"gzip"}
	newZKFromConnectionString = zk.NewFromConnectionString
)

// SyncerConfig is the Syncer's configuration.
type SyncerConfig struct {
	StrSQLMode        *string            `toml:"sql-mode" json:"sql-mode"`
	SQLMode           mysql.SQLMode      `toml:"-" json:"-"`
	IgnoreTxnCommitTS []int64            `toml:"ignore-txn-commit-ts" json:"ignore-txn-commit-ts"`
	IgnoreSchemas     string             `toml:"ignore-schemas" json:"ignore-schemas"`
	IgnoreTables      []filter.TableName `toml:"ignore-table" json:"ignore-table"`
	TxnBatch          int                `toml:"txn-batch" json:"txn-batch"`
	WorkerCount       int                `toml:"worker-count" json:"worker-count"`
	To                *dsync.DBConfig    `toml:"to" json:"to"`
	DoTables          []filter.TableName `toml:"replicate-do-table" json:"replicate-do-table"`
	DoDBs             []string           `toml:"replicate-do-db" json:"replicate-do-db"`
	DestDBType        string             `toml:"db-type" json:"db-type"`
	DisableDispatch   bool               `toml:"disable-dispatch" json:"disable-dispatch"`
	SafeMode          bool               `toml:"safe-mode" json:"safe-mode"`
	DisableCausality  bool               `toml:"disable-detect" json:"disable-detect"`
}

// Config holds the configuration of drainer
type Config struct {
	*flag.FlagSet   `json:"-"`
	LogLevel        string          `toml:"log-level" json:"log-level"`
	NodeID          string          `toml:"node-id" json:"node-id"`
	ListenAddr      string          `toml:"addr" json:"addr"`
	AdvertiseAddr   string          `toml:"advertise-addr" json:"advertise-addr"`
	DataDir         string          `toml:"data-dir" json:"data-dir"`
	DetectInterval  int             `toml:"detect-interval" json:"detect-interval"`
	EtcdURLs        string          `toml:"pd-urls" json:"pd-urls"`
	LogFile         string          `toml:"log-file" json:"log-file"`
	InitialCommitTS int64           `toml:"initial-commit-ts" json:"initial-commit-ts"`
	SyncerCfg       *SyncerConfig   `toml:"syncer" json:"sycner"`
	Security        security.Config `toml:"security" json:"security"`
	SyncedCheckTime int             `toml:"synced-check-time" json:"synced-check-time"`
	Compressor      string          `toml:"compressor" json:"compressor"`
	EtcdTimeout     time.Duration
	MetricsAddr     string
	MetricsInterval int
	configFile      string
	printVersion    bool
	tls             *tls.Config
}

// NewConfig return an instance of configuration
func NewConfig() *Config {
	cfg := &Config{
		EtcdTimeout: defaultEtcdTimeout,
		SyncerCfg:   &SyncerConfig{},
	}
	cfg.FlagSet = flag.NewFlagSet("drainer", flag.ContinueOnError)
	fs := cfg.FlagSet
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage of drainer:")
		fs.PrintDefaults()
	}
	fs.StringVar(&cfg.NodeID, "node-id", "", "the ID of drainer node; if not specified, we will generate one from hostname and the listening port")
	fs.StringVar(&cfg.ListenAddr, "addr", util.DefaultListenAddr(8249), "addr (i.e. 'host:port') to listen on for drainer connections")
	fs.StringVar(&cfg.AdvertiseAddr, "advertise-addr", "", "addr(i.e. 'host:port') to advertise to the public, default to be the same value as -addr")
	fs.StringVar(&cfg.DataDir, "data-dir", defaultDataDir, "drainer data directory path (default data.drainer)")
	fs.IntVar(&cfg.DetectInterval, "detect-interval", defaultDetectInterval, "the interval time (in seconds) of detect pumps' status")
	fs.StringVar(&cfg.EtcdURLs, "pd-urls", defaultEtcdURLs, "a comma separated list of PD endpoints")
	fs.StringVar(&cfg.LogLevel, "L", "info", "log level: debug, info, warn, error, fatal")
	fs.StringVar(&cfg.configFile, "config", "", "path to the configuration file")
	fs.BoolVar(&cfg.printVersion, "V", false, "print version information and exit")
	fs.StringVar(&cfg.MetricsAddr, "metrics-addr", "", "prometheus pushgateway address, leaves it empty will disable prometheus push")
	fs.IntVar(&cfg.MetricsInterval, "metrics-interval", 15, "prometheus client push interval in second, set \"0\" to disable prometheus push")
	fs.StringVar(&cfg.LogFile, "log-file", "", "log file path")
	fs.Int64Var(&cfg.InitialCommitTS, "initial-commit-ts", 0, "if drainer donesn't have checkpoint, use initial commitTS to initial checkpoint")
	fs.StringVar(&cfg.Compressor, "compressor", "", "use the specified compressor to compress payload between pump and drainer, only 'gzip' is supported now (default \"\", ie. compression disabled.)")
	fs.IntVar(&cfg.SyncerCfg.TxnBatch, "txn-batch", 20, "number of binlog events in a transaction batch")
	fs.StringVar(&cfg.SyncerCfg.IgnoreSchemas, "ignore-schemas", "INFORMATION_SCHEMA,PERFORMANCE_SCHEMA,mysql", "disable sync those schemas")
	fs.IntVar(&cfg.SyncerCfg.WorkerCount, "c", 16, "parallel worker count")
	fs.StringVar(&cfg.SyncerCfg.DestDBType, "dest-db-type", "mysql", "target db type: mysql or tidb or file or kafka; see syncer section in conf/drainer.toml")
	fs.BoolVar(&cfg.SyncerCfg.DisableDispatch, "disable-dispatch", false, "disable dispatching sqls that in one same binlog; if set true, work-count and txn-batch would be useless")
	fs.BoolVar(&cfg.SyncerCfg.SafeMode, "safe-mode", false, "enable safe mode to make syncer reentrant")
	fs.BoolVar(&cfg.SyncerCfg.DisableCausality, "disable-detect", false, "disable detect causality")
	fs.IntVar(&maxBinlogItemCount, "cache-binlog-count", defaultBinlogItemCount, "blurry count of binlogs in cache, limit cache size")
	fs.IntVar(&cfg.SyncedCheckTime, "synced-check-time", defaultSyncedCheckTime, "if we can't detect new binlog after many minute, we think the all binlog is all synced")
	fs.StringVar(new(string), "log-rotate", "", "DEPRECATED")

	return cfg
}

func (cfg *Config) String() string {
	data, err := json.MarshalIndent(cfg, "\t", "\t")
	if err != nil {
		log.Error("marshal json failed", zap.Error(err))
	}

	return string(data)
}

// Parse parses all config from command-line flags, environment vars or the configuration file
func (cfg *Config) Parse(args []string) error {
	// parse first to get config file
	perr := cfg.FlagSet.Parse(args)
	switch perr {
	case nil:
	case flag.ErrHelp:
		os.Exit(0)
	default:
		os.Exit(2)
	}
	if cfg.printVersion {
		fmt.Println(version.GetRawVersionInfo())
		os.Exit(0)
	}

	// load config file if specified
	if cfg.configFile != "" {
		if err := cfg.configFromFile(cfg.configFile); err != nil {
			return errors.Trace(err)
		}
	}
	// parse again to replace with command line options
	if err := cfg.FlagSet.Parse(args); err != nil {
		return errors.Trace(err)
	}
	if len(cfg.FlagSet.Args()) > 0 {
		return errors.Errorf("'%s' is not a valid flag", cfg.FlagSet.Arg(0))
	}
	// replace with environment vars
	err := flags.SetFlagsFromEnv("BINLOG_SERVER", cfg.FlagSet)
	if err != nil {
		return errors.Trace(err)
	}

	if cfg.SyncerCfg.StrSQLMode != nil {
		cfg.SyncerCfg.SQLMode, err = mysql.GetSQLMode(*cfg.SyncerCfg.StrSQLMode)
		if err != nil {
			return errors.Annotate(err, "invalid config: `sql-mode` must be a valid SQL_MODE")
		}
	}

	cfg.tls, err = cfg.Security.ToTLSConfig()
	if err != nil {
		return errors.Errorf("tls config %+v error %v", cfg.Security, err)
	}

	if err = cfg.adjustConfig(); err != nil {
		return errors.Trace(err)
	}

	initializeSaramaGlobalConfig()
	return cfg.validate()
}

func (c *SyncerConfig) adjustWorkCount() {
	if c.DestDBType == "file" || c.DestDBType == "kafka" {
		c.DisableDispatch = true
		c.WorkerCount = 1
	} else if c.DisableDispatch {
		c.WorkerCount = 1
	}
}

func (c *SyncerConfig) adjustDoDBAndTable() {
	for i := 0; i < len(c.DoTables); i++ {
		c.DoTables[i].Table = strings.ToLower(c.DoTables[i].Table)
		c.DoTables[i].Schema = strings.ToLower(c.DoTables[i].Schema)
	}
	for i := 0; i < len(c.DoDBs); i++ {
		c.DoDBs[i] = strings.ToLower(c.DoDBs[i])
	}
}

func (cfg *Config) configFromFile(path string) error {
	return util.StrictDecodeFile(path, "drainer", cfg)
}

// validate checks whether the configuration is valid
func (cfg *Config) validate() error {
	if err := validateAddr(cfg.ListenAddr); err != nil {
		return errors.Annotate(err, "invalid addr")
	}
	if err := validateAddr(cfg.AdvertiseAddr); err != nil {
		return errors.Annotate(err, "invalid advertise-addr")
	}

	// check EtcdEndpoints
	if _, err := flags.NewURLsValue(cfg.EtcdURLs); err != nil {
		return errors.Errorf("parse EtcdURLs error: %s, %v", cfg.EtcdURLs, err)
	}

	if cfg.Compressor != "" {
		found := false
		for _, c := range supportedCompressors {
			if cfg.Compressor == c {
				found = true
				break
			}
		}
		if !found {
			return errors.Errorf(
				"Invalid compressor: %v, must be one of these: %v", cfg.Compressor, supportedCompressors)
		}
	}

	return nil
}

func (cfg *Config) adjustConfig() error {
	// adjust configuration
	util.AdjustString(&cfg.ListenAddr, util.DefaultListenAddr(8249))
	util.AdjustString(&cfg.AdvertiseAddr, cfg.ListenAddr)
	cfg.ListenAddr = "http://" + cfg.ListenAddr       // add 'http:' scheme to facilitate parsing
	cfg.AdvertiseAddr = "http://" + cfg.AdvertiseAddr // add 'http:' scheme to facilitate parsing
	util.AdjustString(&cfg.DataDir, defaultDataDir)
	util.AdjustInt(&cfg.DetectInterval, defaultDetectInterval)

	// add default syncer.to configuration if need
	if cfg.SyncerCfg.To == nil {
		cfg.SyncerCfg.To = new(dsync.DBConfig)
	}

	if cfg.SyncerCfg.DestDBType == "pb" {
		// pb is an alias of file, use file instead
		cfg.SyncerCfg.DestDBType = "file"
	}

	if cfg.SyncerCfg.DestDBType == "kafka" {
		// get KafkaAddrs from zookeeper if ZkAddrs is setted
		if cfg.SyncerCfg.To.ZKAddrs != "" {
			zkClient, err := newZKFromConnectionString(cfg.SyncerCfg.To.ZKAddrs, time.Second*5, time.Second*60)
			if err != nil {
				return errors.Trace(err)
			}
			defer zkClient.Close()

			kafkaUrls, err := zkClient.KafkaUrls()
			if err != nil {
				return errors.Trace(err)
			}

			// use kafka address get from zookeeper to reset the config
			log.Info("get kafka addrs from zookeeper", zap.String("kafka urls", kafkaUrls))
			cfg.SyncerCfg.To.KafkaAddrs = kafkaUrls
		}

		if cfg.SyncerCfg.To.KafkaVersion == "" {
			cfg.SyncerCfg.To.KafkaVersion = defaultKafkaVersion
		}
		if cfg.SyncerCfg.To.KafkaAddrs == "" {
			addrs := os.Getenv("KAFKA_ADDRS")
			if len(addrs) > 0 {
				cfg.SyncerCfg.To.KafkaAddrs = addrs
			} else {
				cfg.SyncerCfg.To.KafkaAddrs = defaultKafkaAddrs
			}
		}

		if cfg.SyncerCfg.To.KafkaMaxMessages <= 0 {
			cfg.SyncerCfg.To.KafkaMaxMessages = 1024
		}
	} else if cfg.SyncerCfg.DestDBType == "file" {
		if len(cfg.SyncerCfg.To.BinlogFileDir) == 0 {
			cfg.SyncerCfg.To.BinlogFileDir = cfg.DataDir
			log.Info("use default downstream file directory", zap.String("directory", cfg.DataDir))
		}
	} else if cfg.SyncerCfg.DestDBType == "mysql" || cfg.SyncerCfg.DestDBType == "tidb" {
		if len(cfg.SyncerCfg.To.Host) == 0 {
			host := os.Getenv("MYSQL_HOST")
			if host == "" {
				host = "localhost"
			}
			cfg.SyncerCfg.To.Host = host
		}
		if cfg.SyncerCfg.To.Port == 0 {
			port, _ := strconv.Atoi(os.Getenv("MYSQL_PORT"))
			if port == 0 {
				port = 3306
			}
			cfg.SyncerCfg.To.Port = port
		}
		if len(cfg.SyncerCfg.To.User) == 0 {
			user := os.Getenv("MYSQL_USER")
			if user == "" {
				user = "root"
			}
			cfg.SyncerCfg.To.User = user
		}
		if len(cfg.SyncerCfg.To.Password) == 0 {
			cfg.SyncerCfg.To.Password = os.Getenv("MYSQL_PSWD")
		}
	}

	cfg.SyncerCfg.adjustWorkCount()
	cfg.SyncerCfg.adjustDoDBAndTable()

	return nil
}

func validateAddr(addr string) error {
	urllis, err := url.Parse(addr)
	if err != nil {
		return errors.Annotatef(err, "failed to parse addr %v", addr)
	}

	var host string
	if host, _, err = net.SplitHostPort(urllis.Host); err != nil {
		return errors.Annotatef(err, "invalid host %v", urllis.Host)
	}

	if !util.IsValidateListenHost(host) {
		log.Warn("pump may not be able to access drainer using this addr", zap.String("listen addr", addr))
	}
	return nil
}
