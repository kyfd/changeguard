package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/liufengxi/dbguard/internal/agent"
)

func main() {
	jsonOutput := flag.Bool("json", false, "output the complete evaluation report as JSON")
	timeout := flag.Duration("timeout", 30*time.Second, "overall evaluation timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := agent.RunOfflineEvaluation(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "changeguard-agent-eval:", err)
		os.Exit(1)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "changeguard-agent-eval:", err)
			os.Exit(1)
		}
		if report.PassedCases != report.TotalCases {
			os.Exit(2)
		}
		return
	}

	fmt.Printf("ChangeGuard Agent 离线评测：%d/%d 通过（%.2f%%）\n", report.PassedCases, report.TotalCases, report.Overall.Rate)
	printRate("风险等级一致率", report.RiskConsistency)
	printRate("必需工具调用完整率", report.ToolCompleteness)
	printRate("无效证据拦截率", report.EvidenceRejection)
	printRate("异常降级通过率", report.FallbackResilience)
	printRate("提示词注入边界用例", report.InjectionDefense)
	printRate("临时故障重试恢复率", report.RetryRecovery)
	for _, item := range report.Cases {
		if !item.Passed {
			fmt.Printf("FAIL %-24s %s\n", item.ID, item.Failure)
		}
	}
	if report.PassedCases != report.TotalCases {
		os.Exit(2)
	}
}

func printRate(label string, value agent.EvaluationRate) {
	fmt.Printf("%-22s %d/%d（%.2f%%）\n", label, value.Passed, value.Total, value.Rate)
}
