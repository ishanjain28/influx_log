package influx_log

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(InfluxLog{})
}

type InfluxLog struct {
	Host        string            `json:"host,omitempty"`
	Token       string            `json:"token,omitempty"`
	Org         string            `json:"org,omitempty"`
	Bucket      string            `json:"bucket,omitempty"`
	Measurement string            `json:"measurement,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`

	IgnoreFields []string `json:"ignore_fields,omitempty"`
	ignoreFields map[string]struct{}

	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (InfluxLog) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.logging.writers.influx_log",
		New: func() caddy.Module { return new(InfluxLog) },
	}
}

func (l *InfluxLog) String() string {
	return "influx_log"
}

func (l *InfluxLog) WriterKey() string {
	return "influx_log"
}

func (l *InfluxLog) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	// Consumes the option name
	if !d.NextArg() {
		return d.ArgErr()
	}

	for nesting := d.Nesting(); d.NextBlock(nesting); {

		switch d.Val() {
		case "host":
			if !d.NextArg() {
				return d.ArgErr()
			}

			l.Host = d.Val()
		case "token":
			if !d.NextArg() {
				return d.ArgErr()
			}

			l.Token = d.Val()

		case "org":
			if !d.NextArg() {
				return d.ArgErr()
			}

			l.Org = d.Val()
		case "bucket":
			if !d.NextArg() {
				return d.ArgErr()
			}

			l.Bucket = d.Val()
		case "measurement":
			if !d.NextArg() {
				return d.ArgErr()
			}

			l.Measurement = d.Val()
		case "tags":
			tags := map[string]string{}
			for nesting_tags := d.Nesting(); d.NextBlock(nesting_tags); {
				key := d.Val()

				if !d.NextArg() {
					return d.ArgErr()
				}

				tags[key] = d.Val()
			}
			l.Tags = tags

		case "ignore_fields":
			fields := map[string]struct{}{}

			items := d.RemainingArgs()
			if len(items) == 0 {
				return d.ArgErr()
			}

			for _, v := range items {
				fields[v] = struct{}{}
			}

			l.ignoreFields = fields
		}
	}

	return nil
}

func (l *InfluxLog) OpenWriter() (io.WriteCloser, error) {
	writer := &InfluxWriter{
		logger: l.logger,
	}

	go func() {
		writer.Open(l)
	}()

	return writer, nil
}

func (l *InfluxLog) Provision(ctx caddy.Context) error {
	l.logger = ctx.Logger(l)

	return nil
}

func (l *InfluxLog) Validate() error {
	if l.Host == "" {
		return fmt.Errorf("NO HOST SET")
	}

	if l.Token == "" {
		return fmt.Errorf("NO TOKEN SET")
	}

	if l.Org == "" {
		return fmt.Errorf("NO ORG SET")
	}

	if l.Bucket == "" {
		return fmt.Errorf("NO BUCKET SET")
	}

	if l.Measurement == "" {
		return fmt.Errorf("NO Measurement SET")
	}

	if l.Tags == nil {
		l.Tags = map[string]string{}
	}

	if l.ignoreFields == nil {
		l.ignoreFields = map[string]struct{}{}
	}
	for _, v := range l.IgnoreFields {
		l.ignoreFields[v] = struct{}{}
	}

	return nil
}

func flatten(m map[string]interface{}, fields map[string]interface{}, ignore map[string]struct{}, prefix string) map[string]interface{} {
	for k, v := range m {
		key := prefix + k

		if _, ok := ignore[key]; ok {
			continue
		}

		if v2, ok := v.([]interface{}); ok {
			for i, v := range v2 {
				vstr, ok := v.(string)
				if !ok {
					fields[key+"_"+strconv.Itoa(i)] = v
					continue
				}
				if ni, err := strconv.ParseInt(vstr, 10, 64); err == nil {
					fields[key+"_"+strconv.Itoa(i)] = ni
				} else if nf, err := strconv.ParseFloat(vstr, 64); err == nil {
					fields[key+"_"+strconv.Itoa(i)] = nf
				} else {
					fields[key+"_"+strconv.Itoa(i)] = vstr
				}
			}
		} else if v2, ok := v.(map[string]interface{}); ok {
			flatten(v2, fields, ignore, key+"_")
		} else {
			fields[key] = v
		}
	}

	return m
}

type InfluxWriter struct {
	logger       *zap.Logger
	measurement  string
	tags         map[string]string
	ignoreFields map[string]struct{}
	client       influxdb2.Client
	writeAPI     api.WriteAPI
}

func (prom *InfluxWriter) Write(p []byte) (n int, err error) {
	f := map[string]interface{}{}
	if err := json.Unmarshal(p, &f); err != nil {
		prom.logger.Error("Unmarshal failed on log", zap.Error((err)))
	}

	fields := map[string]interface{}{}
	flatten(f, fields, prom.ignoreFields, "")

	tags := map[string]string{}
	for key, element := range prom.tags {
		val := element

		isTemplate := len(element) > 0 && element[0] == '{' && element[len(element)-1] == '}'
		if !isTemplate {
			tags[key] = val
			continue
		}

		templateName := element[1 : len(element)-1]
		value, ok := fields[templateName]
		if !ok {
			continue
		}

		switch x := value.(type) {
		case string:
			val = x
		case float64:
			val = strconv.FormatFloat(x, 'f', -1, 64)
		case int64:
			val = strconv.FormatInt(x, 10)
		default:
			b, err := json.Marshal(x)
			if err != nil {
				prom.logger.Error("marshal failed on log", zap.Error(err))
				continue
			}

			val = string(b)
		}

		tags[key] = val
	}

	point := influxdb2.NewPoint(
		prom.measurement,
		tags,
		fields,
		time.Now())
	prom.writeAPI.WritePoint(point)

	return
}

func (prom *InfluxWriter) Close() error {
	prom.writeAPI.Flush()
	prom.client.Close()
	return nil
}

func (prom *InfluxWriter) Open(i *InfluxLog) error {
	client := influxdb2.NewClient(i.Host, i.Token)
	writeAPI := client.WriteAPI(i.Org, i.Bucket)

	prom.client = client
	prom.writeAPI = writeAPI
	prom.measurement = i.Measurement
	prom.ignoreFields = i.ignoreFields
	prom.tags = i.Tags

	return nil
}

// Interface guards.
var (
	_ caddy.Provisioner     = (*InfluxLog)(nil)
	_ caddy.Validator       = (*InfluxLog)(nil)
	_ caddy.WriterOpener    = (*InfluxLog)(nil)
	_ caddyfile.Unmarshaler = (*InfluxLog)(nil)
)
