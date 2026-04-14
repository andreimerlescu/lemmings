package main

import (
  "context"
  "fmt"
  "time"

  "github.com/andreimerlescu/figtree/v2"
)

const (
  defaultHit string = "http://localhost:8080/about"
  defaultWith int64 = 369
  defaultUsing int64 = 17
  defaultUntil time.Duration = 144 * time.Second
  defaultRamp time.Duration = 12 * time.Minute
  defaultBoost int = 1
)

type (
  application struct {
    ctx context.Context
    figs figtree.Plant
  }
)

func main() {
  app := application{
    ctx: context.Background(),
  }
  app.figs = figtree.Grow()
  // lemmings -hit <endpoint> -with <quantity> -using <concurrency> -until 3s -ramp 5m -boost -1
  // lemmings -h http://localhost:8080/about -w 10000 -u 100 -u 3s -r 5m -b -1
  
  // existing figtree usage
  app.figs.NewString("hit", defaultHit, "URL lemmings access")
          .WithAlias("hit", "h")
          .WithValidator("hit", figtree.AssureStringNotEmpty)
          .WithValidator("hit", figtree.AssureStringHasPrefix("http"))
  app.figs.NewInt64("with", defaultWith, "Number of lemmings per channel")
          .WithAlias("with", "w")
          .WithValidator("with", figtree.AssureInt64InRange(1,1_000_000))
  app.figs.NewInt64("using", defaultUsing, "Number of channels to send lemmings into")
          .WithAlias("using", "u")
          .WithValidator("using", figtree.AssureInt64InRange(1,1_000_000))
  app.figs.NewDuration("until", defaultFor, "Duration of lemming visit per hit")
          .WithAlias("until", "f")
          .WithValidator("until", figtree.AssureDurationGreaterThan(0.0))
          .WithValidator("until", figtree.AssureDurationLessThan(time.Hour))
  app.figs.NewDuration("ramp", defaultRamp, "Time it takes to get to 50% concurrency")
          .WithAlias("ramp", "r")
          .WithValidator("ramp", figtree.AssureDurationGreaterThan(0.0))    
          .WithValidator("ramp", figtree.AssureDurationLessThan(time.Hour))
  app.figs.NewInt("boost", defaultBoost, "Threads to use for concurrency")
          .WithAlias("boost", "b")
          .WithValidator("boost", figtree.AssureIntInRange(-1,1_000))
  app.figs.NewBool("crawl", defaultCrawl, "When true, crawl up to -crawl-depth links deep")
          .WithAlias("crawl", "c")
  app.figs.NewBool("crawl-depth", defaultCrawlDepth, "How many links deep to crawl")
          .WithAlias("crawl-depth", "cd")
          .WithValidator("crawl-depth", figtree.AssureIntInRange(1,100))
  
  if len(app.figs.Problems()) > 0 {
    for _, problem := range app.figs.Problems() {
      fmt.Println(problem)
    }
  }

  if err := app.figs.Load(); err != nil {
    fmt.Fatalln(err)
  }

  if err := app.Start(); err != nil {
    log.Fatalln(err)
  }

}

func (app *application) Start() error {
  // lemmings main entry start point 

  if err := app.rampUp(); err != nil {
    log.Println(err)
    return err
  }

  
}

func (app *application) rampUp() error {
  ctx, cancel := context.WithTimeout(app.ctx, 5*time.Minute)
  defer cancel()
  for range *app.figs.Int64("using") {
    go app.spawn(*app.figs.String("hit"))
  }
}

func (app *application) spawn(in string) {
  ctx, cancel := context.WithTimeout(app.ctx, *app.figs.UnitDuration("until",time.Second))
  defer cancel()
  if _, err := app.DownloadPath(in); err != nil {
    log.Println(err)
  }
  time.Sleep(*app.figs.UnitDuration("until", time.Seconds))
}

func (app *application) Crawl(start string) error {
  ctx, cancel := context.WithTimeout(app.ctx, 5*time.Minute)
  defer cancel()

  if len(start) == 0 {
    start = *app.figs.String("hit")
  }
  
  hitBytes, bytesErr := app.DownloadPath(start)
  if bytesErr != nil {
    log.Println(bytesErr)
    return bytesErr
  }

  links, extractErr := app.ExtractLinks(hitBytes)
  if extractErr != nil {
    log.Println(extractErr)
    return extractErr
  }

  for _, link := range links {
    err := app.Crawl(link)
    if err != nil {
      log.Println(err)
    }
  }
}

// DownloadPath acquires the bytes from the [in] path and retuns the `[]byte` or error from the endpoint
func (app *application) DownloadPath(in string) ([]byte, error) {

}

// ExtractLinks returns an array of paths that exist in the body of the -hit | -h endpoint
func (app *application) ExtractLinks(in []byte) ([]string, error) {

}

// IndexSitemap returns an array of paths that exist in sitemap.xml
func (app *application) IndexSitemap(in string) ([]string, error) {

}


