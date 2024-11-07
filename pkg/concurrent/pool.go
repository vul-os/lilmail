// pkg/concurrent/pool.go
package concurrent

import (
	"context"
	"sync"
)

// Job represents a unit of work to be done
type Job interface {
	Do(ctx context.Context) error
}

// Pool is a worker pool that processes jobs concurrently
type Pool struct {
	workers int
	jobs    chan Job
	results chan error
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewPool creates a new worker pool with the specified number of workers
func NewPool(workers int) *Pool {
	return &Pool{
		workers: workers,
		jobs:    make(chan Job),
		results: make(chan error),
		done:    make(chan struct{}),
	}
}

// Start begins the worker pool
func (p *Pool) Start(ctx context.Context) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx)
	}
}

// Submit adds a job to the pool
func (p *Pool) Submit(job Job) {
	p.jobs <- job
}

// Results returns a channel that receives job results
func (p *Pool) Results() <-chan error {
	return p.results
}

// Stop gracefully shuts down the pool
func (p *Pool) Stop() {
	close(p.jobs)
	p.wg.Wait()
	close(p.results)
	close(p.done)
}

// worker processes jobs from the pool
func (p *Pool) worker(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			p.results <- job.Do(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// EmailFetchJob represents a job to fetch a single email
type EmailFetchJob struct {
	UID      uint32
	Folder   string
	ClientID string
}

// Do implements the Job interface for EmailFetchJob
func (j *EmailFetchJob) Do(ctx context.Context) error {
	// The actual implementation will be called by the email package
	// This is just the job structure
	return nil
}

// BatchProcessor handles concurrent processing of email batches
type BatchProcessor struct {
	pool      *Pool
	batchSize int
}

// NewBatchProcessor creates a new batch processor
func NewBatchProcessor(workers, batchSize int) *BatchProcessor {
	return &BatchProcessor{
		pool:      NewPool(workers),
		batchSize: batchSize,
	}
}

// ProcessBatch processes a batch of UIDs concurrently
func (b *BatchProcessor) ProcessBatch(ctx context.Context, uids []uint32, folder, clientID string) []error {
	b.pool.Start(ctx)
	defer b.pool.Stop()

	var errors []error
	var errorsMu sync.Mutex

	// Submit jobs in batches
	for i := 0; i < len(uids); i += b.batchSize {
		end := i + b.batchSize
		if end > len(uids) {
			end = len(uids)
		}

		// Process batch
		for _, uid := range uids[i:end] {
			job := &EmailFetchJob{
				UID:      uid,
				Folder:   folder,
				ClientID: clientID,
			}
			b.pool.Submit(job)
		}

		// Collect results for this batch
		for j := i; j < end; j++ {
			if err := <-b.pool.Results(); err != nil {
				errorsMu.Lock()
				errors = append(errors, err)
				errorsMu.Unlock()
			}
		}
	}

	return errors
}
