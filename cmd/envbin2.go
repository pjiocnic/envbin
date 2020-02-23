package main

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	sigar "github.com/cloudfoundry/gosigar"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/klauspost/cpuid"
	"github.com/mxk/go-flowrate/flowrate"
)

var (
	version string
)

type Settings struct {
	delay     int64
	bandwidth int64
	errorRate float64
	cpuUse    float64
	health    bool
	liveness  bool
}

func NewSettings() *Settings {
	return &Settings{
		delay:     0,
		bandwidth: math.MaxInt64,
		errorRate: 0.0,
		health:    true,
		liveness:  true,
	}
}

// Ugly we have to do this and it's not in the library
func loggingMiddleware(next http.Handler) http.Handler {
	return handlers.CombinedLoggingHandler(os.Stdout, next)
}
func recoveryMiddleware(next http.Handler) http.Handler {
	return handlers.RecoveryHandler()(next)
}

func latencyMiddleware(delay *int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// FIXMW: delay can change! Needs a reference to the settings object
		time.Sleep(time.Duration(*delay) * time.Second)
		next.ServeHTTP(w, r)
	})
}

type slowResponseWriter struct {
	rw   http.ResponseWriter
	fw   *flowrate.Writer
	rxbw *int64
	bw   int64
}

func newSlowResponseWriter(rw http.ResponseWriter, bandwidth *int64) slowResponseWriter {
	fw := flowrate.NewWriter(rw, *bandwidth)
	return slowResponseWriter{rw, fw, bandwidth, *bandwidth}
	//defer fw.Close()
}

func (sr slowResponseWriter) Header() http.Header {
	return sr.rw.Header()
}
func (sr slowResponseWriter) Write(b []byte) (written int, err error) {
	// TODO: this should really be a method - make NewBandwidthMiddleware, and the resulting object has a SetBw and MiddleFunc
	if sr.bw != *sr.rxbw {
		// FIXME: not thread safe
		sr.bw = *sr.rxbw
		sr.fw.SetLimit(sr.bw)
		log.Println("adjusted writer bw to ", sr.bw)
	}
	written, err = sr.fw.Write(b)
	sr.rw.(http.Flusher).Flush()
	return
}
func (sr slowResponseWriter) WriteHeader(statusCode int) {
	sr.rw.WriteHeader(statusCode)
}
func bandwidthMiddleware(bandwidth *int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(newSlowResponseWriter(w, bandwidth), r)
	})
}

func errorMiddleware(rate *float64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *rate < rand.Float64() {
			next.ServeHTTP(w, r)
		} else {
			w.WriteHeader(500)
		}
	})
}

var allocs = make([][]byte, 0, 0)

func allocAndTouch(bytes int64) {
	bs := make([]byte, bytes, bytes)
	var i int64
	for i = 0; i < bytes; i += int64(os.Getpagesize()) {
		bs[i] = 69
	}
	allocs = append(allocs, bs)
}

// Not the best "algorithm" in the world.
// * Seems to over/undershoot by about 10%. This could be the sampling rate of top though
// * You really don't need to do anything "cpu-intensive" here; this happily loads 16 virtual cores.
func useCpu(rate *float64) {
	done := make(chan int)

	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			period := time.Tick(1 * time.Second)
			for {
				cpus := float64(runtime.NumCPU())
				// Try to cap the high-time to rought 1s. If it's more than that, then the high duty part lasts longer than the 1s period timer, and that channel starts to fill up with ticks. As soon as there's some breathing room again that'll quickly get drained, but the channel could get full and idk what the Tick producer does then.
				// Also, if the user requests a crazy duty cycle of say 1M, then it won't respond to requests to lower that rate until after a period of 1M / #cores.
				dutyCycle := math.Min(*rate, cpus) / cpus
				highTimer := time.After(
					time.Duration(
						dutyCycle*1000,
					) * time.Millisecond,
				)
			high:
				for {
					select {
					case <-highTimer:
						break high
					default:
					}
				}
				<-period // low
			}
		}()
	}

	time.Sleep(time.Second * 10)
	close(done)
}

func main() {
	startTime := time.Now()
	s := NewSettings()

	root_mux := mux.NewRouter()
	root_mux.Use(loggingMiddleware)

	api_mux := root_mux.PathPrefix("/api").Subrouter()
	api_mux.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api_mux.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
			methods, err := route.GetMethods()
			if err != nil {
				methods = []string{"GET"}
			}
			pathTemplate, err := route.GetPathTemplate()
			if err == nil {
				queriesTemplates, err := route.GetQueriesTemplates()
				if err == nil {
					// TODO: should return JSON? Is there a standard / convention for self-discoverable REST APIs?
					fmt.Fprintf(w, "%s %s?%s\n", methods, pathTemplate, strings.Join(queriesTemplates, ","))
				}
			}
			return nil
		})
	})

	api_mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		rc, err := strconv.ParseInt(mux.Vars(r)["code"], 0, 32)
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			fmt.Fprintf(w, "Exiting %d\n", rc)
			w.(http.Flusher).Flush()
			os.Exit(int(rc))
		}
	}).Methods("POST").Queries("code", "{code:[0-9]+}")

	/* Latency to first byte */
	api_mux.HandleFunc("/delay", func(w http.ResponseWriter, r *http.Request) {
		d, err := strconv.ParseInt(mux.Vars(r)["value"], 0, 64)
		// or: d, err := strconv.ParseInt(r.URL.Query().Get("value"), 0, 64)
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			s.delay = d
			fmt.Fprintf(w, "Delay set to %v\n", d)
		}
	}).Methods("POST").Queries("value", "{value:[0-9]+}")

	/* Latecy between bytes */
	api_mux.HandleFunc("/bandwidth", func(w http.ResponseWriter, r *http.Request) {
		b, err := strconv.ParseInt(mux.Vars(r)["value"], 0, 64)
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			s.bandwidth = b
			fmt.Fprintf(w, "Bandwidth set to %v bytes/s\n", b)
		}
	}).Methods("POST").Queries("value", "{value:[0-9]+}")

	/* Proportion of 500s */
	api_mux.HandleFunc("/errorrate", func(w http.ResponseWriter, r *http.Request) {
		e, err := strconv.ParseFloat(mux.Vars(r)["value"], 0)
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			s.errorRate = e
			fmt.Fprintf(w, "Error rate set to %v\n", e)
		}
	}).Methods("POST").Queries("value", "{value:[0-9.]+}")

	/* Allocate (and use) memory */
	api_mux.HandleFunc("/allocate", func(w http.ResponseWriter, r *http.Request) {
		a, err := strconv.ParseInt(mux.Vars(r)["value"], 0, 64)
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			fmt.Fprintf(w, "Allocating %d bytes\n", a)
			allocAndTouch(a)
		}
	}).Methods("POST").Queries("value", "{value:[0-9]+}")

	/* Use CPU at a given rate */
	api_mux.HandleFunc("/cpu", func(w http.ResponseWriter, r *http.Request) {
		c, err := strconv.ParseFloat(mux.Vars(r)["value"], 0)
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			s.cpuUse = c
			fmt.Fprintf(w, "CPU usage set to %v\n", c)
		}
	}).Methods("POST").Queries("value", "{value:[0-9.]+}")

	api_mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		h, err := strconv.ParseBool(mux.Vars(r)["value"])
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			s.health = h
			fmt.Fprintf(w, "Health check set to %v\n", h)
		}
	}).Methods("POST").Queries("value", "{value:[true|false]}")

	api_mux.HandleFunc("/liveness", func(w http.ResponseWriter, r *http.Request) {
		l, err := strconv.ParseBool(mux.Vars(r)["value"])
		if err != nil {
			fmt.Fprintf(w, "%v\n", err)
		} else {
			s.liveness = l
			fmt.Fprintf(w, "Health check set to %v\n", l)
		}
	}).Methods("POST").Queries("value", "{value:[true|false]}")

	go useCpu(&s.cpuUse)

	root_mux.Handle("/",
		errorMiddleware(&s.errorRate,
			latencyMiddleware(&s.delay,
				bandwidthMiddleware(&s.bandwidth,
					http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hostname, _ := os.Hostname()
						mem := sigar.Mem{}
						mem.Get()
						uptime := sigar.Uptime{}
						uptime.Get()
						procs := sigar.ProcList{}
						procs.Get()
						ms := new(runtime.MemStats)
						runtime.ReadMemStats(ms)

						data := make(map[string]string) //TODO: strongly type me with a struct. Esp for (optional) sections
						data["Version"] = version
						data["GoVersion"] = runtime.Version()
						data["StartTime"] = startTime.Format("2006-01-02 15:04:05")
						data["OsType"] = runtime.GOOS
						data["OsVersion"] = "TODO"
						data["OsUptime"] = uptime.Format()
						data["Pid"] = strconv.Itoa(os.Getpid())
						data["Uid"] = strconv.Itoa(os.Getuid())
						data["Gid"] = strconv.Itoa(os.Getgid())
						data["Arch"] = runtime.GOARCH
						data["CpuName"] = cpuid.CPU.BrandName
						data["PhysCores"] = strconv.Itoa(cpuid.CPU.PhysicalCores)
						data["VirtCores"] = strconv.Itoa(cpuid.CPU.LogicalCores)
						data["MemTotal"] = formatPow2(mem.Total)
						data["ProcCount"] = strconv.Itoa(len(procs.List))
						data["Hostname"] = hostname
						data["Ip"] = getDefaultIp()
						data["MemUseVirtual"] = strconv.Itoa(int(ms.Sys))
						data["MemUsePhysical"] = "TODO"
						data["CpuSelfTime"] = "TODO"
						data["SettingHealth"] = strconv.FormatBool(s.health)
						data["SettingLiveness"] = strconv.FormatBool(s.liveness)
						data["SettingLatency"] = strconv.Itoa(int(s.delay))
						data["SettingBandwidth"] = strconv.Itoa(int(s.bandwidth))

						/* This does the application/text output quite nicely, but for a fancy HTML page we probably want:
						* - gorilla mux SPA example
						* - SPA (react etc) which can be made elsewhere and loaded with gobindata (to avoid the complexity of hosting it behing a separate web server. Or maybe we do, in the same container / Pod?)
						* - JSON api for this struct (make it a struct and JSON serialse it) so it can be read by the SPA
						 */

						// Templates can be executed straight into writers, so we could pump the template into the httpResponseWriter. Problem is, it only flushes on the boundaries into and out of {{}} template substitutions, which makes the output sporadic. So we dump into a string and write that one byte at a time.
						var b bytes.Buffer
						t, err := template.ParseFiles("text.tpl")
						if err != nil {
							log.Fatalf("TODO")
						}
						t.Execute(&b, data)
						bs := b.Bytes()

						for i := 0; i < len(bs); i++ {
							w.Write(bs[i : i+1])
						}
					}),
				),
			),
		),
	)

	root_mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.health {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "ok")
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "error")
		}

	})

	root_mux.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) {
		if s.liveness {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "ok")
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "error")
		}

	})

	log.Print("Listening on :8088")
	log.Fatal(http.ListenAndServe(":8088", root_mux))
}

func getDefaultIp() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		log.Println(err)
		return "<unknown>"
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP.String()
}

func formatPow2(n uint64) string {
	return fmt.Sprintf("%dG", n>>30)
}
