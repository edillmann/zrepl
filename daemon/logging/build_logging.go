package logging

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"github.com/mattn/go-isatty"
	"github.com/pkg/errors"
	"github.com/problame/go-streamrpc"
	"github.com/zrepl/zrepl/config"
	"github.com/zrepl/zrepl/daemon/pruner"
	"github.com/zrepl/zrepl/endpoint"
	"github.com/zrepl/zrepl/logger"
	"github.com/zrepl/zrepl/replication"
	"github.com/zrepl/zrepl/tlsconf"
	"os"
	"github.com/zrepl/zrepl/daemon/snapper"
	"github.com/zrepl/zrepl/daemon/transport/serve"
)

func OutletsFromConfig(in config.LoggingOutletEnumList) (*logger.Outlets, error) {

	outlets := logger.NewOutlets()

	if len(in) == 0 {
		// Default config
		out := WriterOutlet{&HumanFormatter{}, os.Stdout}
		outlets.Add(out, logger.Warn)
		return outlets, nil
	}

	var syslogOutlets, stdoutOutlets int
	for lei, le := range in {

		outlet, minLevel, err := parseOutlet(le)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot parse outlet #%d", lei)
		}
		var _ logger.Outlet = WriterOutlet{}
		var _ logger.Outlet = &SyslogOutlet{}
		switch outlet.(type) {
		case *SyslogOutlet:
			syslogOutlets++
		case WriterOutlet:
			stdoutOutlets++
		}

		outlets.Add(outlet, minLevel)

	}

	if syslogOutlets > 1 {
		return nil, errors.Errorf("can only define one 'syslog' outlet")
	}
	if stdoutOutlets > 1 {
		return nil, errors.Errorf("can only define one 'stdout' outlet")
	}

	return outlets, nil

}

const (
	SubsysReplication = "repl"
	SubsysStreamrpc   = "rpc"
	SubsyEndpoint     = "endpoint"
)

func WithSubsystemLoggers(ctx context.Context, log logger.Logger) context.Context {
	ctx = replication.WithLogger(ctx, log.WithField(SubsysField, "repl"))
	ctx = streamrpc.ContextWithLogger(ctx, streamrpcLogAdaptor{log.WithField(SubsysField, "rpc")})
	ctx = endpoint.WithLogger(ctx, log.WithField(SubsysField, "endpoint"))
	ctx = pruner.WithLogger(ctx, log.WithField(SubsysField, "pruning"))
	ctx = snapper.WithLogger(ctx, log.WithField(SubsysField, "snapshot"))
	ctx = serve.WithLogger(ctx, log.WithField(SubsysField, "serve"))
	return ctx
}

func parseLogFormat(i interface{}) (f EntryFormatter, err error) {
	var is string
	switch j := i.(type) {
	case string:
		is = j
	default:
		return nil, errors.Errorf("invalid log format: wrong type: %T", i)
	}

	switch is {
	case "human":
		return &HumanFormatter{}, nil
	case "logfmt":
		return &LogfmtFormatter{}, nil
	case "json":
		return &JSONFormatter{}, nil
	default:
		return nil, errors.Errorf("invalid log format: '%s'", is)
	}

}

func parseOutlet(in config.LoggingOutletEnum) (o logger.Outlet, level logger.Level, err error) {

	parseCommon := func(common config.LoggingOutletCommon) (logger.Level, EntryFormatter, error) {
		if common.Level == "" || common.Format == "" {
			return 0, nil, errors.Errorf("must specify 'level' and 'format' field")
		}

		minLevel, err := logger.ParseLevel(common.Level)
		if err != nil {
			return 0, nil, errors.Wrap(err, "cannot parse 'level' field")
		}
		formatter, err := parseLogFormat(common.Format)
		if err != nil {
			return 0, nil, errors.Wrap(err, "cannot parse 'formatter' field")
		}
		return minLevel, formatter, nil
	}

	var f EntryFormatter

	switch v := in.Ret.(type) {
	case *config.StdoutLoggingOutlet:
		level, f, err = parseCommon(v.LoggingOutletCommon)
		if err != nil {
			break
		}
		o, err = parseStdoutOutlet(v, f)
	case *config.TCPLoggingOutlet:
		level, f, err = parseCommon(v.LoggingOutletCommon)
		if err != nil {
			break
		}
		o, err = parseTCPOutlet(v, f)
	case *config.SyslogLoggingOutlet:
		level, f, err = parseCommon(v.LoggingOutletCommon)
		if err != nil {
			break
		}
		o, err = parseSyslogOutlet(v, f)
	default:
		panic(v)
	}
	return o, level, err
}

func parseStdoutOutlet(in *config.StdoutLoggingOutlet, formatter EntryFormatter) (WriterOutlet, error) {
	flags := MetadataAll
	writer := os.Stdout
	if !isatty.IsTerminal(writer.Fd()) && !in.Time {
		flags &= ^MetadataTime
	}
	if isatty.IsTerminal(writer.Fd()) && !in.Color {
		flags &= ^MetadataColor
	}

	formatter.SetMetadataFlags(flags)
	return WriterOutlet{
		formatter,
		os.Stdout,
	}, nil
}

func parseTCPOutlet(in *config.TCPLoggingOutlet, formatter EntryFormatter) (out *TCPOutlet, err error) {
	var tlsConfig *tls.Config
	if in.TLS != nil {
		tlsConfig, err = func(m *config.TCPLoggingOutletTLS, host string) (*tls.Config, error) {
			clientCert, err := tls.LoadX509KeyPair(m.Cert, m.Key)
			if err != nil {
				return nil, errors.Wrap(err, "cannot load client cert")
			}

			var rootCAs *x509.CertPool
			if m.CA == "" {
				if rootCAs, err = x509.SystemCertPool(); err != nil {
					return nil, errors.Wrap(err, "cannot open system cert pool")
				}
			} else {
				rootCAs, err = tlsconf.ParseCAFile(m.CA)
				if err != nil {
					return nil, errors.Wrap(err, "cannot parse CA cert")
				}
			}
			if rootCAs == nil {
				panic("invariant violated")
			}

			return tlsconf.ClientAuthClient(host, rootCAs, clientCert)
		}(in.TLS, in.Address)
		if err != nil {
			return nil, errors.New("cannot not parse TLS config in field 'tls'")
		}
	}

	formatter.SetMetadataFlags(MetadataAll)
	return NewTCPOutlet(formatter, in.Net, in.Address, tlsConfig, in.RetryInterval), nil

}

func parseSyslogOutlet(in *config.SyslogLoggingOutlet, formatter EntryFormatter) (out *SyslogOutlet, err error) {
	out = &SyslogOutlet{}
	out.Formatter = formatter
	out.Formatter.SetMetadataFlags(MetadataNone)
	out.RetryInterval = in.RetryInterval
	return out, nil
}
