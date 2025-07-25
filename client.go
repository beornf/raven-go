// Package raven implements a client for the Sentry error logging service.
package raven

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/certifi/gocertifi"
)

const (
	userAgent              = "raven-go/1.0"
	timestampFormat        = `"2006-01-02T15:04:05.00"`
	transportClientTimeout = 30 * time.Second
)

// Internal SDK Error types
var (
	ErrPacketDropped         = errors.New("raven: packet dropped")
	ErrUnableToUnmarshalJSON = errors.New("raven: unable to unmarshal JSON")
	ErrMissingUser           = errors.New("raven: dsn missing public key and/or password")
	ErrMissingProjectID      = errors.New("raven: dsn missing project id")
	ErrInvalidSampleRate     = errors.New("raven: sample rate should be between 0 and 1")
)

// Severity used in the level attribute of a message
type Severity string

// http://docs.python.org/2/howto/logging.html#logging-levels
const (
	DEBUG   = Severity("debug")
	INFO    = Severity("info")
	WARNING = Severity("warning")
	ERROR   = Severity("error")
	FATAL   = Severity("fatal")
)

// Logger used in all internal log calls which can be enabled with SetDebug(true) function call
var debugLogger = log.New(ioutil.Discard, "", 0)

// Timestamp holds the creation time of a Packet
type Timestamp time.Time

// MarshalJSON returns the JSON encoding of a timestamp
func (timestamp Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(time.Time(timestamp).UTC().Format(timestampFormat)), nil
}

// UnmarshalJSON sets timestamp to parsed JSON data
func (timestamp *Timestamp) UnmarshalJSON(data []byte) error {
	t, err := time.Parse(timestampFormat, string(data))
	if err != nil {
		return err
	}

	*timestamp = Timestamp(t)
	return nil
}

// Format return timestamp in configured timestampFormat
func (timestamp Timestamp) Format(format string) string {
	t := time.Time(timestamp)
	return t.Format(format)
}

// An Interface is a Sentry interface that will be serialized as JSON.
// It must implement json.Marshaler or use json struct tags.
type Interface interface {
	// The Sentry class name. Example: sentry.interfaces.Stacktrace
	Class() string
}

// Culpriter holds information about the exception culprit
type Culpriter interface {
	Culprit() string
}

// Transport used in Capture calls that handles communication with the Sentry servers
type Transport interface {
	Send(url, authHeader string, packet *Packet) error
}

// Extra keeps track of any additional information that developer wants to attach to the final packet
type Extra map[string]interface{}

type outgoingPacket struct {
	packet *Packet
	ch     chan error
}

// Tag is a key:value pair of strings provided by user to better categorize events
type Tag struct {
	Key   string
	Value string
}

// Tags keep track of user configured tags
type Tags []Tag

// MarshalJSON returns the JSON encoding of a tag
func (t *Tag) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]string{t.Key, t.Value})
}

// UnmarshalJSON sets tag to parsed JSON data
func (t *Tag) UnmarshalJSON(data []byte) error {
	var tag [2]string
	if err := json.Unmarshal(data, &tag); err != nil {
		return err
	}
	*t = Tag{tag[0], tag[1]}
	return nil
}

// UnmarshalJSON sets tags to parsed JSON data
func (t *Tags) UnmarshalJSON(data []byte) error {
	var tags []Tag

	switch data[0] {
	case '[':
		// Unmarshal into []Tag
		if err := json.Unmarshal(data, &tags); err != nil {
			return err
		}
	case '{':
		// Unmarshal into map[string]string
		tagMap := make(map[string]string)
		if err := json.Unmarshal(data, &tagMap); err != nil {
			return err
		}

		// Convert to []Tag
		for k, v := range tagMap {
			tags = append(tags, Tag{k, v})
		}
	default:
		return ErrUnableToUnmarshalJSON
	}

	*t = tags
	return nil
}

// Packet defines Sentry's spec compliant interface holding Event information (top-level object) - https://docs.sentry.io/development/sdk-dev/attributes/
type Packet struct {
	// Required
	Message string `json:"message"`

	// Required, set automatically by Client.Send/Report via Packet.Init if blank
	EventID   string    `json:"event_id"`
	Project   string    `json:"project"`
	Timestamp Timestamp `json:"timestamp"`
	Level     Severity  `json:"level"`
	Logger    string    `json:"logger"`

	// Optional
	Platform    string            `json:"platform,omitempty"`
	Culprit     string            `json:"culprit,omitempty"`
	ServerName  string            `json:"server_name,omitempty"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Tags        Tags              `json:"tags,omitempty"`
	Modules     map[string]string `json:"modules,omitempty"`
	Fingerprint []string          `json:"fingerprint,omitempty"`
	Extra       Extra             `json:"extra,omitempty"`

	Interfaces []Interface `json:"-"`
}

// NewPacket constructs a packet with the specified message and interfaces.
func NewPacket(message string, interfaces ...Interface) *Packet {
	extra := Extra{}
	setExtraDefaults(extra)
	return &Packet{
		Message:    message,
		Interfaces: interfaces,
		Extra:      extra,
	}
}

// NewPacketWithExtra constructs a packet with the specified message, extra information, and interfaces.
func NewPacketWithExtra(message string, extra Extra, interfaces ...Interface) *Packet {
	if extra == nil {
		extra = Extra{}
	}
	setExtraDefaults(extra)

	return &Packet{
		Message:    message,
		Interfaces: interfaces,
		Extra:      extra,
	}
}

func setExtraDefaults(extra Extra) Extra {
	extra["runtime.Version"] = runtime.Version()
	extra["runtime.NumCPU"] = runtime.NumCPU()
	extra["runtime.GOMAXPROCS"] = runtime.GOMAXPROCS(0) // 0 just returns the current value
	extra["runtime.NumGoroutine"] = runtime.NumGoroutine()
	return extra
}

// Init initializes required fields in a packet. It is typically called by
// Client.Send/Report automatically.
func (packet *Packet) Init(project string) error {
	if packet.Project == "" {
		packet.Project = project
	}
	if packet.EventID == "" {
		var err error
		packet.EventID, err = uuid()
		if err != nil {
			return err
		}
	}
	if time.Time(packet.Timestamp).IsZero() {
		packet.Timestamp = Timestamp(time.Now())
	}
	if packet.Level == "" {
		packet.Level = ERROR
	}
	if packet.Logger == "" {
		packet.Logger = "root"
	}
	if packet.ServerName == "" {
		packet.ServerName = hostname
	}
	if packet.Platform == "" {
		packet.Platform = "go"
	}

	if packet.Culprit == "" {
		for _, inter := range packet.Interfaces {
			if c, ok := inter.(Culpriter); ok {
				packet.Culprit = c.Culprit()
				if packet.Culprit != "" {
					break
				}
			}
		}
	}

	return nil
}

// AddTags appends new tags to the existing ones
func (packet *Packet) AddTags(tags map[string]string) {
	for k, v := range tags {
		packet.Tags = append(packet.Tags, Tag{k, v})
	}
}

func uuid() (string, error) {
	id := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, id)
	if err != nil {
		return "", err
	}
	id[6] &= 0x0F // clear version
	id[6] |= 0x40 // set version to 4 (random uuid)
	id[8] &= 0x3F // clear variant
	id[8] |= 0x80 // set to IETF variant
	return hex.EncodeToString(id), nil
}

// JSON encodes packet into JSON format that will be sent to the server
func (packet *Packet) JSON() ([]byte, error) {
	packetJSON, err := json.Marshal(packet)
	if err != nil {
		return nil, err
	}

	interfaces := make(map[string]Interface, len(packet.Interfaces))
	for _, inter := range packet.Interfaces {
		if inter != nil {
			interfaces[inter.Class()] = inter
		}
	}

	if len(interfaces) > 0 {
		interfaceJSON, err := json.Marshal(interfaces)
		if err != nil {
			return nil, err
		}
		packetJSON[len(packetJSON)-1] = ','
		packetJSON = append(packetJSON, interfaceJSON[1:]...)
	}

	return packetJSON, nil
}

type context struct {
	user *User
	http *Http
	tags map[string]string
}

func (c *context) setUser(u *User) { c.user = u }
func (c *context) setHttp(h *Http) { c.http = h }
func (c *context) setTags(t map[string]string) {
	if c.tags == nil {
		c.tags = make(map[string]string)
	}
	for k, v := range t {
		c.tags[k] = v
	}
}
func (c *context) clear() {
	c.user = nil
	c.http = nil
	c.tags = nil
}

// Return a list of interfaces to be used in appending with the rest
func (c *context) interfaces() []Interface {
	len, i := 0, 0
	if c.user != nil {
		len++
	}
	if c.http != nil {
		len++
	}
	interfaces := make([]Interface, len)
	if c.user != nil {
		interfaces[i] = c.user
		i++
	}
	if c.http != nil {
		interfaces[i] = c.http
	}
	return interfaces
}

// MaxQueueBuffer the maximum number of packets that will be buffered waiting to be delivered.
// Packets will be dropped if the buffer is full. Used by NewClient.
var MaxQueueBuffer = 100

func SetMaxQueueBuffer(maxCount int) {
	MaxQueueBuffer = maxCount
}

func newTransport() Transport {
	t := &HTTPTransport{}
	rootCAs, err := gocertifi.CACerts()
	if err != nil {
		debugLogger.Println("failed to load root TLS certificates:", err)
	} else {
		t.Client = &http.Client{
			Transport: &http.Transport{
				Proxy:           http.ProxyFromEnvironment,
				TLSClientConfig: &tls.Config{RootCAs: rootCAs},
			},
			Timeout: transportClientTimeout,
		}
	}
	return t
}

func newClient(tags map[string]string) *Client {
	client := &Client{
		Transport:  newTransport(),
		Tags:       tags,
		context:    &context{},
		sampleRate: 1.0,
		queue:      make(chan *outgoingPacket, MaxQueueBuffer),
	}
	err := client.SetDSN(os.Getenv("SENTRY_DSN"))

	if err != nil {
		debugLogger.Println("incorrect DSN", err)
	}

	client.SetRelease(os.Getenv("SENTRY_RELEASE"))
	client.SetEnvironment(os.Getenv("SENTRY_ENVIRONMENT"))
	return client
}

// New constructs a new Sentry client instance
func New(dsn string) (*Client, error) {
	client := newClient(nil)
	return client, client.SetDSN(dsn)
}

// NewWithTags constructs a new Sentry client instance with default tags.
func NewWithTags(dsn string, tags map[string]string) (*Client, error) {
	client := newClient(tags)
	return client, client.SetDSN(dsn)
}

// NewClient constructs a Sentry client and spawns a background goroutine to
// handle packets sent by Client.Report.
//
// Deprecated: use New and NewWithTags instead
func NewClient(dsn string, tags map[string]string) (*Client, error) {
	client := newClient(tags)
	return client, client.SetDSN(dsn)
}

// Client encapsulates a connection to a Sentry server. It must be initialized
// by calling NewClient. Modification of fields concurrently with Send or after
// calling Report for the first time is not thread-safe.
type Client struct {
	Tags map[string]string

	Transport Transport

	// DropHandler is called when a packet is dropped because the buffer is full.
	DropHandler func(*Packet)

	// Context that will get appending to all packets
	context *context

	mu          sync.RWMutex
	url         string
	projectID   string
	authHeader  string
	release     string
	environment string
	sampleRate  float32

	// default logger name (leave empty for 'root')
	defaultLoggerName string

	includePaths       []string
	ignoreErrorsRegexp *regexp.Regexp
	queue              chan *outgoingPacket

	// A WaitGroup to keep track of all currently in-progress captures
	// This is intended to be used with Client.Wait() to assure that
	// all messages have been transported before exiting the process.
	wg sync.WaitGroup

	// A Once to track only starting up the background worker once
	start sync.Once
}

// DefaultClient initialize a default *Client instance
var DefaultClient = newClient(nil)

// SetIgnoreErrors updates ignoreErrors config on given client
func (client *Client) SetIgnoreErrors(errs []string) error {
	joinedRegexp := strings.Join(errs, "|")
	r, err := regexp.Compile(joinedRegexp)
	if err != nil {
		return fmt.Errorf("raven: failed to compile regexp %q for %q: %v", joinedRegexp, errs, err)
	}

	client.mu.Lock()
	client.ignoreErrorsRegexp = r
	client.mu.Unlock()
	return nil
}

func (client *Client) shouldExcludeErr(errStr string) bool {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.ignoreErrorsRegexp != nil && client.ignoreErrorsRegexp.MatchString(errStr)
}

// SetIgnoreErrors updates ignoreErrors config on default client
func SetIgnoreErrors(errs ...string) error {
	return DefaultClient.SetIgnoreErrors(errs)
}

// SetDSN updates a client with a new DSN. It safe to call after and
// concurrently with calls to Report and Send.
func (client *Client) SetDSN(dsn string) error {
	if dsn == "" {
		return nil
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	uri, err := url.Parse(dsn)
	if err != nil {
		return err
	}

	if uri.User == nil {
		return ErrMissingUser
	}
	publicKey := uri.User.Username()
	secretKey, hasSecretKey := uri.User.Password()
	uri.User = nil

	if idx := strings.LastIndex(uri.Path, "/"); idx != -1 {
		client.projectID = uri.Path[idx+1:]
		uri.Path = uri.Path[:idx+1] + "api/" + client.projectID + "/store/"
	}
	if client.projectID == "" {
		return ErrMissingProjectID
	}

	client.url = uri.String()

	if hasSecretKey {
		client.authHeader = fmt.Sprintf("Sentry sentry_version=4, sentry_key=%s, sentry_secret=%s", publicKey, secretKey)
	} else {
		client.authHeader = fmt.Sprintf("Sentry sentry_version=4, sentry_key=%s", publicKey)
	}

	return nil
}

// SetDSN sets the DSN for the default *Client instance
func SetDSN(dsn string) error { return DefaultClient.SetDSN(dsn) }

// SetRelease sets the "release" tag.
func (client *Client) SetRelease(release string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.release = release
}

// SetEnvironment sets the "environment" tag.
func (client *Client) SetEnvironment(environment string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.environment = environment
}

// SetDefaultLoggerName sets the default logger name.
func (client *Client) SetDefaultLoggerName(name string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.defaultLoggerName = name
}

// SetSampleRate sets how much sampling we want on client side
func (client *Client) SetSampleRate(rate float32) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if rate < 0 || rate > 1 {
		return ErrInvalidSampleRate
	}
	client.sampleRate = rate
	return nil
}

func (client *Client) SetDebug(debug bool) {
	if debug == true {
		debugLogger = log.New(os.Stdout, "raven: ", 0)
	} else {
		debugLogger = log.New(ioutil.Discard, "", 0)
	}
}

// SetRelease sets the "release" tag on the default *Client
func SetRelease(release string) { DefaultClient.SetRelease(release) }

// SetEnvironment sets the "environment" tag on the default *Client
func SetEnvironment(environment string) { DefaultClient.SetEnvironment(environment) }

// SetDefaultLoggerName sets the "defaultLoggerName" on the default *Client
func SetDefaultLoggerName(name string) {
	DefaultClient.SetDefaultLoggerName(name)
}

// SetSampleRate sets the "sample rate" on the degault *Client
func SetSampleRate(rate float32) error { return DefaultClient.SetSampleRate(rate) }

// SetDebug sets the "debug" config on the default *Client
func SetDebug(debug bool) { DefaultClient.SetDebug(debug) }

func (client *Client) worker() {
	for outgoingPacket := range client.queue {

		client.mu.RLock()
		url, authHeader := client.url, client.authHeader
		client.mu.RUnlock()

		outgoingPacket.ch <- client.Transport.Send(url, authHeader, outgoingPacket.packet)
		client.wg.Done()
	}
}

// Capture asynchronously delivers a packet to the Sentry server. It is a no-op
// when client is nil. A channel is provided if it is important to check for a
// send's success.
func (client *Client) Capture(packet *Packet, captureTags map[string]string) (eventID string, ch chan error) {
	ch = make(chan error, 1)

	if client == nil {
		// return a chan that always returns nil when the caller receives from it
		close(ch)
		return
	}

	if client.sampleRate < 1.0 && mrand.Float32() > client.sampleRate {
		return
	}

	if packet == nil {
		close(ch)
		return
	}

	if client.shouldExcludeErr(packet.Message) {
		return
	}

	// Keep track of all running Captures so that we can wait for them all to finish
	// *Must* call client.wg.Done() on any path that indicates that an event was
	// finished being acted upon, whether success or failure
	client.wg.Add(1)

	// Merge capture tags and client tags
	packet.AddTags(captureTags)
	packet.AddTags(client.Tags)

	// Initialize any required packet fields
	client.mu.RLock()
	packet.AddTags(client.context.tags)
	projectID := client.projectID
	release := client.release
	environment := client.environment
	defaultLoggerName := client.defaultLoggerName
	client.mu.RUnlock()

	// set the global logger name on the packet if we must
	if packet.Logger == "" && defaultLoggerName != "" {
		packet.Logger = defaultLoggerName
	}

	// Set Severity if value is provided
	if Severity(captureTags["level"]) != "" {
		packet.Level = Severity(captureTags["level"])
	}

	err := packet.Init(projectID)
	if err != nil {
		ch <- err
		client.wg.Done()
		return
	}

	if packet.Release == "" {
		packet.Release = release
	}

	if packet.Environment == "" {
		packet.Environment = environment
	}

	outgoingPacket := &outgoingPacket{packet, ch}

	// Lazily start background worker until we
	// do our first write into the queue.
	client.start.Do(func() {
		go client.worker()
	})

	select {
	case client.queue <- outgoingPacket:
	default:
		// Send would block, drop the packet
		if client.DropHandler != nil {
			client.DropHandler(packet)
		}
		ch <- ErrPacketDropped
		client.wg.Done()
	}

	return packet.EventID, ch
}

// Capture asynchronously delivers a packet to the Sentry server with the default *Client.
// It is a no-op when client is nil. A channel is provided if it is important to check for a
// send's success.
func Capture(packet *Packet, captureTags map[string]string) (eventID string, ch chan error) {
	return DefaultClient.Capture(packet, captureTags)
}

// CaptureMessage formats and delivers a string message to the Sentry server.
func (client *Client) CaptureMessage(message string, tags map[string]string, interfaces ...Interface) string {
	if client == nil {
		return ""
	}

	if client.shouldExcludeErr(message) {
		return ""
	}

	packet := NewPacket(message, append(append(interfaces, client.context.interfaces()...), &Message{message, nil})...)
	eventID, _ := client.Capture(packet, tags)

	return eventID
}

// CaptureMessage formats and delivers a string message to the Sentry server with the default *Client
func CaptureMessage(message string, tags map[string]string, interfaces ...Interface) string {
	return DefaultClient.CaptureMessage(message, tags, interfaces...)
}

// CaptureMessageAndWait is identical to CaptureMessage except it blocks and waits for the message to be sent.
func (client *Client) CaptureMessageAndWait(message string, tags map[string]string, interfaces ...Interface) string {
	if client == nil {
		return ""
	}

	if client.shouldExcludeErr(message) {
		return ""
	}

	packet := NewPacket(message, append(append(interfaces, client.context.interfaces()...), &Message{message, nil})...)
	eventID, ch := client.Capture(packet, tags)
	if eventID != "" {
		<-ch
	}

	return eventID
}

// CaptureMessageAndWait is identical to CaptureMessage except it blocks and waits for the message to be sent.
func CaptureMessageAndWait(message string, tags map[string]string, interfaces ...Interface) string {
	return DefaultClient.CaptureMessageAndWait(message, tags, interfaces...)
}

// CaptureError formats and delivers an error to the Sentry server.
// Adds a stacktrace to the packet, excluding the call to this method.
func (client *Client) CaptureError(err error, tags map[string]string, interfaces ...Interface) string {
	if client == nil {
		return ""
	}

	if err == nil {
		return ""
	}

	if client.shouldExcludeErr(err.Error()) {
		return ""
	}

	extra := extractExtra(err)
	cause := Cause(err)

	packet := NewPacketWithExtra(err.Error(), extra, append(append(interfaces, client.context.interfaces()...), NewException(cause, GetOrNewStacktrace(cause, 1, 3, client.includePaths)))...)
	eventID, _ := client.Capture(packet, tags)

	return eventID
}

// CaptureError formats and delivers an error to the Sentry server using the default *Client.
// Adds a stacktrace to the packet, excluding the call to this method.
func CaptureError(err error, tags map[string]string, interfaces ...Interface) string {
	return DefaultClient.CaptureError(err, tags, interfaces...)
}

// CaptureErrorAndWait is identical to CaptureError, except it blocks and assures that the event was sent
func (client *Client) CaptureErrorAndWait(err error, tags map[string]string, interfaces ...Interface) string {
	if client == nil {
		return ""
	}

	if client.shouldExcludeErr(err.Error()) {
		return ""
	}

	extra := extractExtra(err)
	cause := Cause(err)

	packet := NewPacketWithExtra(err.Error(), extra, append(append(interfaces, client.context.interfaces()...), NewException(cause, GetOrNewStacktrace(cause, 1, 3, client.includePaths)))...)
	eventID, ch := client.Capture(packet, tags)
	if eventID != "" {
		<-ch
	}

	return eventID
}

// CaptureErrorAndWait is identical to CaptureError, except it blocks and assures that the event was sent
func CaptureErrorAndWait(err error, tags map[string]string, interfaces ...Interface) string {
	return DefaultClient.CaptureErrorAndWait(err, tags, interfaces...)
}

// CapturePanic calls f and then recovers and reports a panic to the Sentry server if it occurs.
// If an error is captured, both the error and the reported Sentry error ID are returned.
func (client *Client) CapturePanic(f func(), tags map[string]string, interfaces ...Interface) (err interface{}, errorID string) {
	// Note: This doesn't need to check for client, because we still want to go through the defer/recover path
	// Down the line, Capture will be noop'd, so while this does a _tiny_ bit of overhead constructing the
	// *Packet just to be thrown away, this should not be the normal case. Could be refactored to
	// be completely noop though if we cared.
	defer func() {
		var packet *Packet
		err = recover()
		switch rval := err.(type) {
		case nil:
			return
		case error:
			if client.shouldExcludeErr(rval.Error()) {
				return
			}
			packet = NewPacket(rval.Error(), append(append(interfaces, client.context.interfaces()...), NewException(rval, NewStacktrace(2, 3, client.includePaths)))...)
		default:
			rvalStr := fmt.Sprint(rval)
			if client.shouldExcludeErr(rvalStr) {
				return
			}
			packet = NewPacket(rvalStr, append(append(interfaces, client.context.interfaces()...), NewException(errors.New(rvalStr), NewStacktrace(2, 3, client.includePaths)))...)
		}

		errorID, _ = client.Capture(packet, tags)
	}()

	f()
	return
}

// CapturePanic calls f and then recovers and reports a panic to the Sentry server if it occurs.
// If an error is captured, both the error and the reported Sentry error ID are returned.
func CapturePanic(f func(), tags map[string]string, interfaces ...Interface) (interface{}, string) {
	return DefaultClient.CapturePanic(f, tags, interfaces...)
}

// CapturePanicAndWait is identical to CapturePanic, except it blocks and assures that the event was sent
func (client *Client) CapturePanicAndWait(f func(), tags map[string]string, interfaces ...Interface) (err interface{}, errorID string) {
	// Note: This doesn't need to check for client, because we still want to go through the defer/recover path
	// Down the line, Capture will be noop'd, so while this does a _tiny_ bit of overhead constructing the
	// *Packet just to be thrown away, this should not be the normal case. Could be refactored to
	// be completely noop though if we cared.
	defer func() {
		var packet *Packet
		err = recover()
		switch rval := err.(type) {
		case nil:
			return
		case error:
			if client.shouldExcludeErr(rval.Error()) {
				return
			}
			packet = NewPacket(rval.Error(), append(append(interfaces, client.context.interfaces()...), NewException(rval, NewStacktrace(2, 3, client.includePaths)))...)
		default:
			rvalStr := fmt.Sprint(rval)
			if client.shouldExcludeErr(rvalStr) {
				return
			}
			packet = NewPacket(rvalStr, append(append(interfaces, client.context.interfaces()...), NewException(errors.New(rvalStr), NewStacktrace(2, 3, client.includePaths)))...)
		}

		var ch chan error
		errorID, ch = client.Capture(packet, tags)
		if errorID != "" {
			<-ch
		}
	}()

	f()
	return
}

// CapturePanicAndWait is identical to CapturePanic, except it blocks and assures that the event was sent
func CapturePanicAndWait(f func(), tags map[string]string, interfaces ...Interface) (interface{}, string) {
	return DefaultClient.CapturePanicAndWait(f, tags, interfaces...)
}

// Close given clients event queue
func (client *Client) Close() {
	close(client.queue)
}

// Close defaults client event queue
func Close() { DefaultClient.Close() }

// Wait blocks and waits for all events to finish being sent to Sentry server
func (client *Client) Wait() {
	client.wg.Wait()
}

// Wait blocks and waits for all events to finish being sent to Sentry server
func Wait() { DefaultClient.Wait() }

// URL returns configured url of given client
func (client *Client) URL() string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.url
}

// URL returns configured url of default client
func URL() string { return DefaultClient.URL() }

// ProjectID returns configured ProjectID of given client
func (client *Client) ProjectID() string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.projectID
}

// ProjectID returns configured ProjectID of default client
func ProjectID() string { return DefaultClient.ProjectID() }

// Release returns configured Release of given client
func (client *Client) Release() string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.release
}

// Release returns configured Release of default client
func Release() string { return DefaultClient.Release() }

// IncludePaths returns configured includePaths of given client
func (client *Client) IncludePaths() []string {
	client.mu.RLock()
	defer client.mu.RUnlock()

	return client.includePaths
}

// IncludePaths returns configured includePaths of default client
func IncludePaths() []string { return DefaultClient.IncludePaths() }

// SetIncludePaths updates includePaths config on given client
func (client *Client) SetIncludePaths(p []string) {
	client.mu.Lock()
	defer client.mu.Unlock()

	client.includePaths = p
}

// SetIncludePaths updates includePaths config on default client
func SetIncludePaths(p []string) { DefaultClient.SetIncludePaths(p) }

// SetUserContext updates User of Context interface on given client
func (client *Client) SetUserContext(u *User) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.context.setUser(u)
}

// SetHttpContext updates Http of Context interface on given client
func (client *Client) SetHttpContext(h *Http) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.context.setHttp(h)
}

// SetTagsContext updates Tags of Context interface on given client
func (client *Client) SetTagsContext(t map[string]string) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.context.setTags(t)
}

// ClearContext clears Context interface on given client by removing tags, user and request information
func (client *Client) ClearContext() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.context.clear()
}

// SetUserContext updates User of Context interface on default client
func SetUserContext(u *User) { DefaultClient.SetUserContext(u) }

// SetHttpContext updates Http of Context interface on default client
func SetHttpContext(h *Http) { DefaultClient.SetHttpContext(h) }

// SetTagsContext updates Tags of Context interface on default client
func SetTagsContext(t map[string]string) { DefaultClient.SetTagsContext(t) }

// ClearContext clears Context interface on default client by removing tags, user and request information
func ClearContext() { DefaultClient.ClearContext() }

// HTTPTransport is the default transport, delivering packets to Sentry via the
// HTTP API.
type HTTPTransport struct {
	*http.Client
}

// Send uses HTTPTransport to send a Packet to configured Sentry's DSN endpoint
func (t *HTTPTransport) Send(url, authHeader string, packet *Packet) error {
	if url == "" {
		return nil
	}

	body, contentType, contentEncoding, err := serializedPacket(packet)
	if err != nil {
		return fmt.Errorf("raven: error serializing packet: %v", err)
	}
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return fmt.Errorf("raven: can't create new request: %v", err)
	}
	req.Header.Set("X-Sentry-Auth", authHeader)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", contentType)
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}

	res, err := t.Do(req)
	if err != nil {
		return err
	}

	// Response body needs to be drained and closed in order for TCP connection to stay opened (via keep-alive) and reused
	_, err = io.Copy(ioutil.Discard, res.Body)
	if err != nil {
		debugLogger.Println("Error while reading response body", res)
	}

	err = res.Body.Close()
	if err != nil {
		debugLogger.Println("Error while closing response body", err)
	}

	if res.StatusCode != 200 {
		return fmt.Errorf("raven: got http status %d - x-sentry-error: %s", res.StatusCode, res.Header.Get("X-Sentry-Error"))
	}
	return nil
}

func serializedPacket(packet *Packet) (io.Reader, string, string, error) {
	packetJSON, err := packet.JSON()
	if err != nil {
		return nil, "", "", fmt.Errorf("raven: error marshaling packet %+v to JSON: %v", packet, err)
	}

	// Only deflate the packet if it is bigger than 1KB, as there is an overhead
	if len(packetJSON) > 1000 {
		buf := &bytes.Buffer{}
		deflate, _ := zlib.NewWriterLevel(buf, zlib.BestCompression)
		_, err := deflate.Write(packetJSON)
		if err != nil {
			debugLogger.Println("Error while deflating data in packet serializer", err)
		}
		err = deflate.Close()
		if err != nil {
			debugLogger.Println("Error while closing zlib deflate in packet serializer", err)
		}
		return buf, "application/octet-stream", "deflate", nil
	}
	return bytes.NewReader(packetJSON), "application/json", "", nil
}

var hostname string

func init() {
	hostname, _ = os.Hostname()
}

// Cause returns the underlying cause of the error, if possible.
// An error value has a cause if it implements the following
// interface:
//
//     type causer interface {
//            Cause() error
//     }
//
// If the error does not implement Cause, the original error will
// be returned.
//
// If the cause of the error is nil, then the original
// error will be returned.
//
// If the error is nil, nil will be returned without further
// investigation.
//
// Will return the deepest cause which is not nil.
func Cause(err error) error {
	type causer interface {
		Cause() error
	}

	for err != nil {
		cause, ok := err.(causer)
		if !ok {
			break
		}

		if _cause := cause.Cause(); _cause != nil {
			err = _cause
		} else {
			break
		}

	}
	return err
}
