# port forward
golang 实现的端口转发工具，支持udp、tcp

# 使用方法
```
支持多个分组，用,号隔开。
参数规则 {tcp|udp}:{本地ip}:{本地端口}-{目标域名或ip}:{目标端口}
比如：
go run main.go -f udp::53-8.8.8.8:53,tcp:0.0.0.0:80-x.x.x.x:80
```