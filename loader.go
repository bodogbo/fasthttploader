package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"time"

	"github.com/hagen1778/fasthttploader/metrics"
	"github.com/hagen1778/fasthttploader/pushgateway"
	"github.com/hagen1778/fasthttploader/report"
	"golang.org/x/time/rate"
)

const (
	// Duration of burst-testing, without qps-limit. Used to estimate start test conditions
	calibrateDuration = 5 * time.Second

	// Duration of adjustable testing, while trying to reach max qps with minimal lvl of errors
	adjustmentDuration = 10 * time.Second

	// Period of sample taking, while testing
	samplePeriod = 500 * time.Millisecond
)

var (
	ctx = context.Background()

	// client do http requests, populate metrics
	client *metrics.Client

	// report represent html-report
	r *report.Page

	// errors storage of errors amount in current step. Used to compare changes in errors-metric
	errors uint64

	// multiplier is a coefficient of qps multiplying during tests
	multiplier = float64(0.1)

	throttle = rate.NewLimiter(1, 1)
	stopCh   = make(chan struct{})
)

type loadConfig struct {
	// qps is the rate limit.
	qps rate.Limit

	// c is a number of workers (clients)
	c int
}

func run() {
	client = metrics.New(req, *t)
	pushgateway.Init()
	r = &report.Page{
		Title:           string(req.URI().Host()),
		RequestDuration: make(map[float64][]float64),
		Interval:        samplePeriod.Seconds(),
	}

	cfg := loadConfig{}
	if *q == 0 {
		fmt.Println("Run burst-load phase")
		burstThroughput(&cfg)

		fmt.Println("Run calibrate phase")
		calibrateThroughput(&cfg)
	} else {
		cfg.qps = rate.Limit(*q)
		cfg.c = *c
	}

	fmt.Println("Run load phase")
	makeLoad(&cfg)

	makeReport()
}

func burstThroughput(cfg *loadConfig) {
	startTime := time.Now()
	timeout := time.After(calibrateDuration)
	client.RunWorkers(*c)
	for {
		select {
		case <-timeout:
			cfg.qps = rate.Limit(float64(metrics.RequestSum()) / calibrateDuration.Seconds())
			cfg.c = client.Amount()
			if (metrics.Errors()/metrics.RequestSum())*100 > 2 { // just more than 3% of errors
				cfg.qps /= 2
				cfg.c /= 2
			}
			printSummary("Burst Throughput", startTime)
			client.Flush()
			return
		default:
			client.Jobsch <- struct{}{}
		}
	}
}

func calibrateThroughput(cfg *loadConfig) {
	t := time.Now()
	client.RunWorkers(cfg.c)
	throttle.SetLimit(cfg.qps)

	go func() {
		timeout := time.After(adjustmentDuration)
		tick := time.Tick(samplePeriod)
		for {
			select {
			case <-timeout:
				stopCh <- struct{}{}
				cfg.qps = throttle.Limit()
				cfg.c = client.Amount()
				printSummary("Adjustment test", t)
				return
			case <-tick:
				printState()
				calibrate()
			}
		}
	}()

	load()
}

func makeLoad(cfg *loadConfig) {
	startTime := time.Now()

	s := time.Duration(d.Seconds() / 2 / 10)
	stepTick := time.Tick(time.Second * s) // half of the time, 10 steps in first half
	stateTick := time.Tick(samplePeriod)

	workerStep := cfg.c / 10
	qpsStep := cfg.qps / 10
	throttle.SetLimit(qpsStep)
	client.RunWorkers(workerStep)
	go func() {
		timeout := time.After(*d)
		steps := 0
		for {
			select {
			case <-timeout:
				stopCh <- struct{}{}
				printSummary("Loading test", startTime)
				return
			case <-stepTick:
				if steps >= 10-1 {
					continue
				}
				throttle.SetLimit(throttle.Limit() + qpsStep)
				client.RunWorkers(workerStep)
				steps++
			case <-stateTick:
				printState()

				//if err := pushgateway.Push(c.Metrics()); err != nil {
				//	fmt.Printf("%s\n", err)
				//}
			}
		}
	}()
	load()
}

var await = 0

func calibrate() {
	if math.Abs(multiplier) < 0.0001 {
		fmt.Println("Multiplier is negligible now. Stoping calibrate")
		stopCh <- struct{}{}
		return
	}

	if await > 0 {
		await -= 1
		return
	}

	if !isFlawed() {
		if client.Overflow() > 0 {
			n := int(float64(client.Amount()) * multiplier)
			client.RunWorkers(n)
			await += 1
		} else {
			throttle.SetLimit(throttle.Limit() * rate.Limit(1+multiplier))
			await += 1
		}
	} else {
		multiplier /= 1.2
		await += 3
	}
}

func printState() {
	if *debug {
		fmt.Println("------------")
		fmt.Printf("[ Multiplier = %f ]\n", multiplier)
		fmt.Printf("QPS was increased to: %f\nWorkers: %d\nJobsch len: %d\n", throttle.Limit(), client.Amount(), client.Overflow())
		fmt.Printf(" >> Num of cons: %d; Req done: %d; Errors: %d; Timeouts: %d\n", metrics.ConnOpen(), metrics.RequestSum(), metrics.Errors(), metrics.Timeouts())
		//fmt.Printf(" >> Real Req/s: %f; Transfer/s: %f kb;\n", float64(metrics.RequestSum())/since, float64(metrics.BytesWritten())/(since*1024))
		fmt.Println("------------")
	}

	r.Lock()
	r.Connections = append(r.Connections, metrics.ConnOpen())
	r.Errors = append(r.Errors, metrics.Errors())
	r.Timeouts = append(r.Timeouts, metrics.Timeouts())
	r.RequestSum = append(r.RequestSum, metrics.RequestSum())
	r.RequestSuccess = append(r.RequestSuccess, metrics.RequestSuccess())
	r.BytesWritten = append(r.BytesWritten, metrics.BytesWritten())
	r.BytesRead = append(r.BytesRead, metrics.BytesRead())
	r.Qps = append(r.Qps, uint64(throttle.Limit()))
	r.UpdateRequestDuration(metrics.RequestDuration())
	r.Unlock()
}

func isFlawed() bool {
	if metrics.Errors() > 0 && errors != metrics.Errors() {
		errors = metrics.Errors()
		return true
	}

	return false
}

func load() {
	for {
		select {
		case <-stopCh:
			client.Flush()
			return
		default:
			if err := throttle.Wait(ctx); err != nil {
				fmt.Println(err)
			}
			client.Jobsch <- struct{}{}
		}
	}
}

func makeReport() {
	f, err := os.Create(*fileName)
	if err != nil {
		log.Fatalf("Error while trying to create file: %s", err)
	}
	defer f.Close()

	f.WriteString(report.PrintPage(r))
	fmt.Printf("Check test results at %s\n", *fileName)
}

func printSummary(stage string, t time.Time) {
	since := time.Since(t).Seconds()
	fmt.Printf("\n------ %s ------\n", stage)
	fmt.Printf("Elapsed time: %fs\n", since)
	fmt.Printf("Req done: %d; Success: %f %%\n", metrics.RequestSum(), (float64(metrics.RequestSuccess())/float64(metrics.RequestSum()))*100)
	fmt.Printf("Rps: %f; Connections: %d\n", float64(metrics.RequestSum())/since, metrics.ConnOpen())
	fmt.Printf("Errors: %d; Timeouts: %d\n\n", metrics.Errors(), metrics.Timeouts())
}
