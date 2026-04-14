# Lemmings

> Simulating real world NPC traffic during high load simulated events.

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/lemmings.svg)](https://pkg.go.dev/github.com/andreimerlescu/lemmings)
[![Go Report Card](https://goreportcard.com/badge/github.com/andreimerlescu/lemmings)](https://goreportcard.com/report/github.com/andreimerlescu/lemmings)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Lemmings is a Go utility that simulates NPC traffic for real world scenario testing.

## Usage

```bash
lemmings -hit <endpoint> -with <quantity> -using <concurrency> -for 3s -ramp 5m -boost -1

lemmings -h http://localhost:8080/about -w 10000 -u 100 -f 3s -r 5m -b -1
```

## Properties

| Property | Type | Alias | Default | Description |
|----------|------|-------|---------|-------------|
| `hit` | `String` | `h` | `http://localhost:8080/` | URL endpoint to reach during test |
| `with` | `Int64` | `w` | `369` | Number of "lemmings" in each "lane" |
| `using` | `Int64` | `u` | `144` | Number of "lanes" of "lemmings" hitting during test |
| `for` | `time.Duration` | `f` | `500 * time.Millisecond` | Duration each "lemming" hit |
| `ramp` | `time.Duration` | `r` | `1 * time.Minute` | duration to ramp up `-using` |
| `boost` | `Int64` | `b` | `1` | Boost controls the channel allocation size* |
| `crawl` | `bool` | N/A | `false` | Analyze `-hit` | `-h` for links and build a local map of successful links |

> \* allowing you to control performance on large or small systems 

## Dependencies 

| Package | Usage |
|---------|-------|
| [figtree](https://github.com/andreimerlescu/figtree) | cli configuration management |
| [sema](https://github.com/andreimerlescu/sema) | primitive runtime control mechanism |

## Runtime 

When the `lemmings` are called forth, they are provided with `-hit` | `-h` for a destination to reach. This can be remote destinations including local destinations using `http://localhost:8080` or `https://example.com`. The package is going to set forth `-with` | `-w` a preferred number of `lemmings` to populate each channel that is going to _hit_ the `-hit` | `-h`. This number allows you to control how long the test will go on for. If you expect 5,000,000 users over 2 hours, then you can easily plan for that with `lemmings`. When you know what your test parameters are, you specify it `-using` | `-u` a number that indicates the quantity of channels that you'll be _hitting_ the `-hit` | `-h` `-for` | `-f` _literally **for**_ a `time.Duration` that is parsed as a `String` but can be either the `int64` value or something like "3s" or "5m" or "1h3m2s" etc. If you want to, you can enable `-ramp` which will calculate the total number of hits and it'll ramp them up. 

Here are some common combinations: 

```bash
lemmings -hit "http://localhost:8080/about" -with 100 -using 10 -for 10s 
```

This means that 10 lanes of 100 where each `lemming` that times out, spends `10s` timing out. So, its `100*10 = 1,000 * 10s = 10,000s = 166.67 min = 2.778 hr. 

What automatically gets written \[if possible\] to disk is the `lemmings.YYYY.MM.DD.md` report that gets generated. If it cannot be saved to disk, then its always saved to STDOUT. When saved to disk, the STDOUT indicates the path of the saved asset and its size (including the directory's presence of other lemmings files by virtue of how many. 

In that report you see: 

```md
lemming v1.0.0 hit http://localhost:8080/about with 100 lemmings using 10 goroutines for 10 seconds per lemming hit visit

total lemmings: 1,000
fastest request: 3ms
slowest request: 69ms
99% Percentile: 4ms
98% Percentile: 4ms
// etc

<path>: 997 2XXs | 2 4XXs | 1 5XX 
```

If `-ramp` | `-r` is enabled in the request, the concurrency is `10` given this example. The test's duration can be calculated knowing the parameters values provided, and this mathematicaly relationship to the program enables `lemmings` to be predictable and auditable in a manner that businesses and organizations can trust its results. 

When `-ramp` | `-r` is enabled, with a value of like `500` for `30s`, enabling _ramp_ intelligently allows your application to boot up and down. This is particularly useful if you're testing the scaling up/down of a containerized microservice architecture and you need components to come online in a particular order before you permit traffic to egress into the architecture itself. Well, what _ramp_ allows you to do is essentially say to the test, "we're going to run this promotion at 8PM EST and we know that the test will need to be for 15 minutes - how fast can we scale after _our_ commercial gets aired? Well, the answer to that hypothetical question is answered in your `lemmings` report. Using _ramp_, you're telling `lemmings` that you want 250 lemmings to be sustained for the latter 50% of the test and then a linear growth of 1 lemming -> 2 lemmings -> 3 lemmings -> 5 lemmings -> 7 lemmings -> (fibonnaci sequence here) -> \[exact remaining\] lemmings before the math works out that 50% remains left to be sustained to the end of the test. The exact value will be a `float64` value that should have the ability to be observed for its rate of performance and efficiency. 

The _ramp_ allows you to get a natural feeling for how long it will hit that, say `5m` ramp up time on a 2hr test. That means that when you announce it, you expect that traffic is going to be sustained on your site at high volume for at least 2 hours at a tune that warrants understanding these metrics directly. 

Combined with [room](https://github.com/andreimerlescu/room), the [lemmings](https://github.com/andreimerlescu/lemmings) package provides you with the ability to load test your application or service for a specific advertisement event. In the rise of AI systems helping everybody build software, these open source free tools provide you with the means necessary to measure your website's deployed performance using real-world human-like lemming-NPC behavior driven actions simulating hitting your site. 

## Sitemap Usage

Some websites follow a convention to provide a sitemap of their pages that they serve. When available, the `lemmings` package will assign each lemming a new route to hit that it will attempt to reach. In the event that a `-hit` | `-h` has a _sitemap_, then `lemmings` will automatically use that to rotate through hits and display the results. When `lemmings` uses a _sitemap_, it's going to divide the number of `lemmings` provided to each lane and assign them evenly to each endpoint. If the _sitemap_ lists 10 destinations and we're sending 500 `lemmings` per lane, then that means that the `lemmings` app will automatically send 50 `lemmings` to each destination in the _sitemap_ itself. With this information, your test informs you in the rendered file as well. 

Each path is expressed with a line in the output file as `/<subpath>: # 2XXs | # 3XXs | # 4XXs | #5XXs`. This provides the `lemmings` the capability to send traffic to a given `-hit` but be intelligent enough to know if the hit has a _sitemap_ available to it to leverage directly and explicitly. 

## Crawling Usage

Some sites are not maintained with an active sitemap.xml and when that happens, the `-crawl` method allows a goroutine process to runs for up to 5 minutes or no new pages with new links or when its considered fully indexed without brute forcing paths. 

The cralwer is designed to designate lemmings to go from point -> point -> point in their journey. 

Each `lemming` will hit each page either in the _sitemap_ or in the _crawler_ such that your coverage is considered real world. 

## Callbacks and event lifecycle

The `lemmings` package itself comes in two forms. The `package` and the `application`. While the application will use `package` itself, it'll consume the lifecycle and call-backs offered at the `package` level that allow you to plug into **prometheus** and other observers. Specifically what the event lifecycle will simulate is the per-lemming events that get triggered on failures, on redirects, etc. so that you can plug into `lemmings` as you see fit at a package level. 






