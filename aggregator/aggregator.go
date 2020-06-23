package aggregator

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	metrics "github.com/Dieterbe/go-metrics"
	"github.com/graphite-ng/carbon-relay-ng/clock"
	"github.com/graphite-ng/carbon-relay-ng/stats"
	log "github.com/sirupsen/logrus"
)

type Aggregator struct {
	Fun          string `json:"fun"`
	procConstr   func(val float64, ts uint32) Processor
	in           chan msg       `json:"-"` // incoming metrics, already split in 3 fields
	out          chan []byte    // outgoing metrics
	Regex        string         `json:"regex,omitempty"`
	Prefix       string         `json:"prefix,omitempty"`
	Sub          string         `json:"substring,omitempty"`
	regex        *regexp.Regexp // compiled version of Regex
	prefix       []byte         // automatically generated based on Prefix or regex, for fast preMatch
	substring    []byte         // based on Sub, for fast preMatch
	OutFmt       string
	outFmt       []byte
	Cache        bool
	reCache      map[string]CacheEntry
	reCacheMutex sync.Mutex
	Interval     uint                          // expected interval between values in seconds, we will quantize to make sure alginment to interval-spaced timestamps
	Wait         uint                          // seconds to wait after quantized time value before flushing final outcome and ignoring future values that are sent too late.
	DropRaw      bool                          // drop raw values "consumed" by this aggregator
	tsList       []uint                        // ordered list of quantized timestamps, so we can flush in correct order
	aggregations map[uint]map[string]Processor // aggregations in process: one for each quantized timestamp and output key, i.e. for each output metric.
	snapReq      chan bool                     // chan to issue snapshot requests on
	snapResp     chan *Aggregator              // chan on which snapshot response gets sent
	shutdown     chan struct{}                 // chan used internally to shut down
	wg           sync.WaitGroup                // tracks worker running state
	now          func() time.Time              // returns current time. wraps time.Now except in some unit tests
	tick         <-chan time.Time              // controls when to flush

	Key        string
	numIn      metrics.Counter
	numFlushed metrics.Counter
}

type msg struct {
	buf [][]byte
	val float64
	ts  uint32
}

// regexToPrefix inspects the regex and returns the longest static prefix part of the regex
// all inputs for which the regex match, must have this prefix
func regexToPrefix(regex string) []byte {
	substr := ""
	for i := 0; i < len(regex); i++ {
		ch := regex[i]
		if i == 0 {
			if ch == '^' {
				continue // good we need this
			} else {
				break // can't deduce any substring here
			}
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			substr += string(ch)
			// "\." means a dot character
		} else if ch == 92 && i+1 < len(regex) && regex[i+1] == '.' {
			substr += "."
			i += 1
		} else {
			//fmt.Println("don't know what to do with", string(ch))
			// anything more advanced should be regex syntax that is more permissive and hence not a static substring.
			break
		}
	}
	return []byte(substr)
}

// New creates an aggregator
func New(fun, regex, prefix, sub, outFmt string, cache bool, interval, wait uint, dropRaw bool, out chan []byte) (*Aggregator, error) {
	ticker := clock.AlignedTick(time.Duration(interval)*time.Second, time.Duration(wait)*time.Second, 2)
	return NewMocked(fun, regex, prefix, sub, outFmt, cache, interval, wait, dropRaw, out, 2000, time.Now, ticker)
}

func NewMocked(fun, regex, prefix, sub, outFmt string, cache bool, interval, wait uint, dropRaw bool, out chan []byte, inBuf int, now func() time.Time, tick <-chan time.Time) (*Aggregator, error) {
	regexObj, err := regexp.Compile(regex)
	if err != nil {
		return nil, err
	}
	procConstr, err := GetProcessorConstructor(fun)
	if err != nil {
		return nil, err
	}

	a := &Aggregator{
		Fun:          fun,
		procConstr:   procConstr,
		in:           make(chan msg, inBuf),
		out:          out,
		Regex:        regex,
		Sub:          sub,
		regex:        regexObj,
		substring:    []byte(sub),
		OutFmt:       outFmt,
		outFmt:       []byte(outFmt),
		Cache:        cache,
		Interval:     interval,
		Wait:         wait,
		DropRaw:      dropRaw,
		aggregations: make(map[uint]map[string]Processor),
		snapReq:      make(chan bool),
		snapResp:     make(chan *Aggregator),
		shutdown:     make(chan struct{}),
		now:          now,
		tick:         tick,
	}
	if prefix != "" {
		a.prefix = []byte(prefix)
		a.Prefix = prefix
	} else {
		a.prefix = regexToPrefix(regex)
		a.Prefix = string(a.prefix)
	}
	if cache {
		a.reCache = make(map[string]CacheEntry)
	}
	a.setKey()
	a.numIn = stats.Counter("unit=Metric.direction=in.aggregator=" + a.Key)
	a.numFlushed = stats.Counter("unit=Metric.direction=out.aggregator=" + a.Key)
	a.wg.Add(1)
	go a.run()
	return a, nil
}

type TsSlice []uint

func (p TsSlice) Len() int           { return len(p) }
func (p TsSlice) Less(i, j int) bool { return p[i] < p[j] }
func (p TsSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func (a *Aggregator) setKey() string {
	h := md5.New()
	h.Write([]byte(a.Fun))
	h.Write([]byte("\000"))
	h.Write([]byte(a.Regex))
	h.Write([]byte("\000"))
	h.Write([]byte(a.Prefix))
	h.Write([]byte("\000"))
	h.Write([]byte(a.Sub))
	h.Write([]byte("\000"))
	h.Write([]byte(a.OutFmt))

	key := fmt.Sprintf("%x", h.Sum(nil))
	a.Key = key[:7]
	return a.Key
}

func (a *Aggregator) AddOrCreate(key string, ts uint32, quantized uint, value float64) bool {
	rangeTracker.Sample(ts)
	aggByKey, ok := a.aggregations[quantized]
	var proc Processor
	if ok {
		proc, ok = aggByKey[key]
		if ok {
			// if both levels exist, we can just add the value and that's it
			proc.Add(value, ts)
		}
	} else {
		// first level doesn't exist. create it and add the ts to the list
		// (second level will be created below)
		a.tsList = append(a.tsList, quantized)
		if len(a.tsList) > 1 && a.tsList[len(a.tsList)-2] > quantized {
			sort.Sort(TsSlice(a.tsList))
		}
		a.aggregations[quantized] = make(map[string]Processor)
	}
	if !ok {
		// note, we only flush where for a given value of now, quantized < now-wait
		// this means that as long as the clock doesn't go back in time
		// we never recreate a previously created bucket (and reflush with same key and ts)
		// a consequence of this is, that if your data stream runs consistently significantly behind
		// real time, it may never be included in aggregates, but it's up to you to configure your wait
		// parameter properly. You can use the rangeTracker and numTooOld metrics to help with this
		if quantized > uint(a.now().Unix())-a.Wait {
			proc = a.procConstr(value, ts)
			a.aggregations[quantized][key] = proc
			return true
		}
		numTooOld.Inc(1)
		return false
	}
	return true
}

// Flush finalizes and removes aggregations that are due
func (a *Aggregator) Flush(cutoff uint) {
	flushWaiting.Inc(1)
	flushes.Add()
	flushWaiting.Dec(1)
	defer flushes.Done()

	pos := -1 // will track the pos of the last ts position that was successfully processed
	for i, ts := range a.tsList {
		if ts > cutoff {
			break
		}
		for key, proc := range a.aggregations[ts] {
			results, ok := proc.Flush()
			if ok {
				if len(results) == 1 {
					a.out <- []byte(fmt.Sprintf("%s %f %d", key, results[0].val, ts))
					a.numFlushed.Inc(1)
				} else {
					for _, result := range results {
						a.out <- []byte(fmt.Sprintf("%s.%s %f %d", key, result.fcnName, result.val, ts))
						a.numFlushed.Inc(1)
					}
				}
			}
		}
		delete(a.aggregations, ts)
		pos = i
	}
	// now we must delete all the timestamps from the ordered list
	if pos == -1 {
		// we didn't process anything, so no action needed
		return
	}
	if pos == len(a.tsList)-1 {
		// we went through all of them. can just reset the slice
		a.tsList = a.tsList[:0]
		return
	}

	// adjust the slice to only contain the timestamps that still need processing,
	// reusing the backing array
	copy(a.tsList[0:], a.tsList[pos+1:])
	a.tsList = a.tsList[:len(a.tsList)-pos-1]

	//fmt.Println("flush done for ", a.now().Unix(), ". agg size now", len(a.aggregations), a.now())
}

func (a *Aggregator) Shutdown() {
	close(a.shutdown)
	a.wg.Wait()
}

func (a *Aggregator) AddMaybe(buf [][]byte, val float64, ts uint32) bool {
	if !a.PreMatch(buf[0]) {
		return false
	}

	if a.DropRaw {
		_, ok := a.matchWithCache(buf[0])
		if !ok {
			return false
		}
	}

	a.in <- msg{
		buf,
		val,
		ts,
	}

	return a.DropRaw
}

//PreMatch checks if the specified metric matches the specified prefix and/or substring
//If prefix isn't explicitly specified it will be derived from the regex where possible.
//If this returns false the metric will not be passed through to the main regex matching stage.
func (a *Aggregator) PreMatch(buf []byte) bool {
	if len(a.prefix) > 0 && !bytes.HasPrefix(buf, a.prefix) {
		return false
	}
	if len(a.substring) > 0 && !bytes.Contains(buf, a.substring) {
		return false
	}
	return true
}

type CacheEntry struct {
	match bool
	key   string
	seen  uint32
}

//
func (a *Aggregator) match(key []byte) (string, bool) {
	var dst []byte
	matches := a.regex.FindSubmatchIndex(key)
	if matches == nil {
		return "", false
	}
	return string(a.regex.Expand(dst, a.outFmt, key, matches)), true
}

// matchWithCache returns whether there was a match, and under which key, if so.
func (a *Aggregator) matchWithCache(key []byte) (string, bool) {
	if a.reCache == nil {
		return a.match(key)
	}

	a.reCacheMutex.Lock()

	var outKey string
	var ok bool
	entry, ok := a.reCache[string(key)]
	if ok {
		entry.seen = uint32(a.now().Unix())
		a.reCache[string(key)] = entry
		a.reCacheMutex.Unlock()
		return entry.key, entry.match
	}

	outKey, ok = a.match(key)

	a.reCache[string(key)] = CacheEntry{
		ok,
		outKey,
		uint32(a.now().Unix()),
	}
	a.reCacheMutex.Unlock()

	return outKey, ok
}

func (a *Aggregator) run() {
	for {
		select {
		case msg := <-a.in:
			// note, we rely here on the fact that the packet has already been validated
			outKey, ok := a.matchWithCache(msg.buf[0])
			if !ok {
				continue
			}
			a.numIn.Inc(1)
			ts := uint(msg.ts)
			quantized := ts - (ts % a.Interval)
			hasTooOld := !a.AddOrCreate(outKey, msg.ts, quantized, msg.val)
			if hasTooOld {
				log.Warnf("Aggregator is receiving too old. key is %v, ts is %v, quantized is %v, value is %v.", string(msg.buf[0]), msg.ts, quantized, msg.val)
			}
		case now := <-a.tick:
			thresh := now.Add(-time.Duration(a.Wait) * time.Second)
			a.Flush(uint(thresh.Unix()))

			// if cache is enabled, clean it out of stale entries
			// it's not ideal to block our channel while flushing AND cleaning up the cache
			// ideally, these operations are interleaved in time, but we can optimize that later
			// this is a simple heuristic but should make the cache always converge on only active data (without memory leaks)
			// even though some cruft may temporarily linger a bit longer.
			// WARNING: this relies on Go's map implementation detail which randomizes iteration order, in order for us to reach
			// the entire keyspace. This may stop working properly with future go releases.  Will need to come up with smth better.
			if a.reCache != nil {
				cutoff := uint32(now.Add(-100 * time.Duration(a.Wait) * time.Second).Unix())
				a.reCacheMutex.Lock()
				for k, v := range a.reCache {
					if v.seen < cutoff {
						delete(a.reCache, k)
					} else {
						break // stop looking when we don't see old entries. we'll look again soon enough.
					}
				}
				a.reCacheMutex.Unlock()
			}
		case <-a.snapReq:
			aggs := make(map[uint]map[string]Processor)
			for quant := range a.aggregations {
				aggs[quant] = make(map[string]Processor)
				for key := range a.aggregations[quant] {
					aggs[quant][key] = nil
				}
			}
			s := &Aggregator{
				Fun:          a.Fun,
				procConstr:   a.procConstr,
				Regex:        a.Regex,
				Prefix:       a.Prefix,
				Sub:          a.Sub,
				prefix:       a.prefix,
				substring:    a.substring,
				OutFmt:       a.OutFmt,
				Cache:        a.Cache,
				Interval:     a.Interval,
				Wait:         a.Wait,
				DropRaw:      a.DropRaw,
				aggregations: aggs,
				now:          time.Now,
				Key:          a.Key,
			}
			a.snapResp <- s
		case <-a.shutdown:
			thresh := a.now().Add(-time.Duration(a.Wait) * time.Second)
			a.Flush(uint(thresh.Unix()))
			a.wg.Done()
			return

		}
	}
}

// to view the state of the aggregator at any point in time
func (a *Aggregator) Snapshot() *Aggregator {
	a.snapReq <- true
	return <-a.snapResp
}
