package engine

// 上行并发调参：默认值与 app EngineTuningConfig 一致；PML_* 环境变量仅用于本机实测扫拐点，
// 生产不设即为默认，零行为变化。app 靠"掉速紧急补偿"(ADD/REMOVE)把并发在活源间再分配，
// adaptive==1 时复刻同一反馈；==0 退回纯静态均分。
import (
	"os"
	"strconv"
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

var dlBudget = 96

// upPaceMbps is the aggregate upload pacing target in Mbps (app tw.Z0 pacing,
// tw.java:776). 0 = off (send as fast as the sink drains). A positive value caps
// each connection to (target / liveConns) so a CDN edge is never handed a t0 flood,
// making the measured upload equal the sustained uplink the app reports. Default 150
// Mbps reproduces the app's steady ~80-100 reading on a typical home uplink and kills
// the t0 absorb burst; raise or zero it via PML_UP_PACE_MBPS on faster links.
var upPaceMbps int64 = 150

func init() {
	dlBudget = envInt("PML_DL_BUDGET", dlBudget)
	dlMaxPerUrl = envInt("PML_DL_MAXPER", dlMaxPerUrl)
	upBudget = envInt("PML_UP_BUDGET", upBudget)
	upMinWorkers = envInt("PML_UP_MINW", upMinWorkers)
	upMaxWorkers = envInt("PML_UP_MAXW", upMaxWorkers)
	maxUp = envInt("PML_MAXUP", maxUp)
	adaptive = envInt("PML_ADAPTIVE", adaptive)
	upPaceMbps = int64(envInt("PML_UP_PACE_MBPS", int(upPaceMbps)))
}
