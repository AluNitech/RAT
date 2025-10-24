module modernrat-adminshell

go 1.23

require (
	github.com/chzyer/readline v1.5.1
	golang.org/x/term v0.25.0
	google.golang.org/grpc v1.67.1
	modernrat-client v0.0.0
)

require (
	golang.org/x/net v0.28.0 // indirect
	golang.org/x/sys v0.26.0 // indirect
	golang.org/x/text v0.17.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

replace modernrat-client => ../go-client
