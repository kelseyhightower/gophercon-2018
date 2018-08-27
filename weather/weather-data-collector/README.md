# weather-data

```
gcloud pubsub topics create weather-events
```

```
gsutil mb gs://weather-app-config
```


```
gcloud alpha functions call weather-data-collector \
  --data '{"event": "GopherCon", "location": "Denver, Colorado, USA"}'
```

```
gcloud alpha functions call weather-data-collector \
  --data '{"event": "Florida Golang", "location": "Orlando, Florida, USA"}'
```

```
gcloud alpha functions call weather-data-collector \
  --data '{"event": "Go Northwest", "location": "Seattle, Washington, USA"}'
```

```
gcloud alpha functions call weather-data-collector \
  --data '{"event": "GothamGo", "location": "New York, New York, USA"}'
```

```
gcloud alpha functions call weather-data-collector \
  --data '{"event": "CapitalGo", "location": "Arlington, Virginia, USA"}'
```

```
gcloud alpha functions call weather-data-collector \
  --data '{"event": "Gopherpalooza", "location": "San Francisco, California, USA"}'
```
