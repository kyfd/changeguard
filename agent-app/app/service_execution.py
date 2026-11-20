"""AgentService 受管理执行监督、检查点恢复与账本追踪 Mixin。"""

from __future__ import annotations

import asyncio
import uuid
from datetime import datetime, timezone
from typing import Any

from app.budget import PHASE_GENERATE, LedgerPersistError, UsageLedger, usage_scope
from app.knowledge import visible_chunks
from app.retrieval.corpus import build_retriever
from app.schemas.drafts import TaskStatus, TaskView
from app.service_common import (
    INTERRUPTED_STATUSES,
    PERSIST_FAILED,
    PERSIST_PERSISTED,
    PERSIST_SKIPPED,
    RESUME_INTERRUPT,
    TaskCancelRejected,
    TaskNotResumable,
    TaskStateUnavailable,
    _Execution,
    _ExecutionProblem,
    _annotate_recovery,
    _delete_thread,
    _effective_requirement,
    _first_interrupt_value,
    _invalidate_stale_confirmations,
    _merge_event_lists,
    _record_draft_version,
)
from app.tools.business import Toolbox
from app.tools.registry import TrustedContext
from app.workflow.checkpoint import has_checkpoint, open_checkpointer
from app.workflow.graph import DraftWorkflow, WorkflowDeps
from app.workflow.state import event


class ExecutionSupervisorMixin:
    """受管理后台/同步执行、终态持久化重试与检查点恢复。"""

    async def cancel(self, task_id: str, context: TrustedContext) -> TaskView:
        record = self._authorize(task_id, context)
        self._ensure_active(record)
        superseded_execution_id = record.get("execution_id") or ""

        # 先在**副本**上构造取消后的状态，并且先落盘。
        #
        # 顺序是关键：如果先终止执行、再落盘，一旦落盘失败就会留下一个
        # 「执行已经停了、仓储却仍是 RECEIVED」的孤儿——没有任何协程在推动它，
        # 健康检查还显示一切正常，clarify 又因为状态不允许而拒绝。
        # 所以落盘失败时必须什么都没发生，调用方拿到显式失败并可以重试。
        candidate = dict(record)
        candidate["run_generation"] = int(record.get("run_generation") or 0) + 1
        candidate["execution_id"] = ""
        candidate["status"] = TaskStatus.CANCELLED.value
        events = list(record.get("events") or [])
        events.append(event("cancelled", "任务被取消，后续步骤不再执行"))
        candidate["events"] = events

        # 落盘前确认没有被并发接管。`TaskRepository.save` 是同步的，且这里到落盘之间
        # 没有 await，因此在单进程事件循环里这一段是原子的（不会被其它协程插入）。
        latest = self._repository.get(task_id)
        if latest is not None and (latest.get("execution_id") or "") != superseded_execution_id:
            raise TaskNotResumable("执行已被新的执行接管，取消未生效，请重试")

        try:
            self._repository.save(candidate)
        except Exception as error:  # noqa: BLE001 - 任何落盘失败都必须变成显式的可重试失败
            raise TaskCancelRejected(
                f"取消未生效：状态未能落盘（{type(error).__name__}），任务仍在运行，请重试"
            ) from error

        # 落盘成功之后才终止执行并清理句柄，而且只清理**刚才那一个**执行。
        self._terminate_superseded_execution(task_id, superseded_execution_id)
        return self._view(candidate)

    async def wait_for(self, task_id: str, context: TrustedContext) -> TaskView:
        """等待后台执行结束（测试与演示用）。

        先校验归属再等待：否则等待本身就会变成一条无授权的存在性探测通道。

        执行结束后若发现它没能留下可查询的终态（存储不可用），必须显式失败，
        而不是把仓储里那份可能过期的记录当成"当前状态"返回。
        """
        self._authorize(task_id, context)
        execution = self._executions.get(task_id)
        if execution is not None:
            await execution.finished.wait()
        problem = self._problems.get(task_id)
        if problem is not None:
            raise TaskStateUnavailable(problem.detail)
        return self._view(self._authorize(task_id, context))

    # -- 内部 --------------------------------------------------------------

    async def _dispatch_or_fail(self, task_id: str, resume: dict[str, Any] | None = None) -> None:
        """派发执行；失败时把任务标为失败。

        否则记录会永远停在 `RECEIVED`：没有任何协程在推动它，也没有超时兜底，
        调用方看到的是一个"一直在运行"的任务。
        """
        try:
            await self._dispatch(task_id, resume)
        except (TaskStateUnavailable, TaskCancelRejected):
            # 存储本身已经出问题，再去写一条 FAILED 只会再失败一次并掩盖真正的原因。
            # 这类失败由调用方显式返回，健康状态另行报告降级。
            raise
        except Exception as error:
            record = self._repository.get(task_id)
            # 只有确实没跑起来（仍在途状态）才改状态，避免覆盖执行中已写入的终态。
            if record is not None and record.get("status") in INTERRUPTED_STATUSES:
                record["status"] = TaskStatus.FAILED.value
                record["error"] = f"任务未能启动：{type(error).__name__}: {error}"
                events = list(record.get("events") or [])
                events.append(event("dispatch_failed", record["error"]))
                record["events"] = events
                self._repository.save(record)
            raise

    async def _dispatch(self, task_id: str, resume: dict[str, Any] | None = None) -> None:
        """按配置决定等待完成还是立即返回。

        两种模式**共用同一套受管理执行**：都登记可取消的句柄，
        区别只在于是不是在这里等它结束。这是 inline 也能被真正取消的前提。
        """
        execution_id = self._begin_execution(task_id)
        if execution_id is None:
            return
        execution = self._start_execution(task_id, execution_id, resume)
        if self._settings.execution_mode == "inline":
            await self._await_execution(execution)

    def _start_execution(self, task_id: str, execution_id: str, resume: dict[str, Any] | None = None) -> _Execution:
        """把一次执行登记成独立的受管理任务。

        独立成 task 而不是直接 await 的好处：调用方的取消（客户端断开）与
        工作流的取消可以分开处理，且句柄始终可被 `cancel` 找到。
        """
        task = asyncio.create_task(
            self._run_managed(task_id, execution_id, resume), name=f"agent-exec:{task_id}:{execution_id[:8]}"
        )
        execution = _Execution(task_id=task_id, execution_id=execution_id, task=task, finished=asyncio.Event())
        self._executions[task_id] = execution
        # 用回调统一收口：既取走异常（否则只会以 "Task exception was never retrieved"
        # 的形式出现在日志里，没有任何机制能查询到），也负责释放句柄。
        task.add_done_callback(lambda finished: self._on_execution_done(execution, finished))
        return execution

    def _on_execution_done(self, execution: _Execution, task: asyncio.Task[None]) -> None:
        if not task.cancelled():
            error = task.exception()
            if error is not None:
                self._record_problem(
                    execution.task_id,
                    execution.execution_id,
                    "exception",
                    f"执行异常终止，状态未能确定：{type(error).__name__}: {error}",
                )
        # 释放句柄时必须校验身份，避免误删后来的新执行。
        if self._executions.get(execution.task_id) is execution:
            self._executions.pop(execution.task_id, None)
        execution.finished.set()

    async def _await_execution(self, execution: _Execution) -> None:
        """等待受管理执行结束，并把它的失败如实向上暴露。"""
        try:
            await execution.finished.wait()
        except asyncio.CancelledError:
            # 调用方自己被取消（例如 inline 请求断开）。内部执行不能变成孤儿：
            # 先终止它，再留下确定的终态，然后继续向上传播取消。
            self._terminate_execution(execution)
            self._abandon_execution(execution, "调用方取消：请求被中断，执行不再继续")
            raise
        problem = self._problems.get(execution.task_id)
        if problem is not None and problem.execution_id == execution.execution_id:
            raise TaskStateUnavailable(problem.detail)

    async def _run_managed(
        self, task_id: str, execution_id: str, resume: dict[str, Any] | None = None
    ) -> None:
        """受管理执行的顶层入口：保证任何退出路径都不留无监督的在途任务。"""
        try:
            record = await self._execute(task_id, execution_id, resume)
        except asyncio.CancelledError:
            # 取消的落盘由**发起方**负责，这里不写：
            #   - 用户 cancel()：先落盘 CANCELLED，再取消执行；
            #   - inline 调用方被取消：由 _await_execution 落盘 FAILED。
            # 两处都不在这里，避免出现第二个写入者互相覆盖。
            return
        if record is None:
            return  # 已被取消或新执行接管：不写才是对的
        outcome = await self._persist_terminal(task_id, execution_id, record)
        if outcome == PERSIST_FAILED:
            self._record_problem(
                task_id,
                execution_id,
                "persist",
                "执行已完成，但终态未能落盘：存储不可用，任务状态无法确定",
            )

    async def _persist_terminal(self, task_id: str, execution_id: str, record: dict[str, Any]) -> str:
        """带**有上限**重试的终态落盘，返回 persisted / skipped / failed。

        只对终态做重试：一次性的存储抖动不应该把一个已经跑完的任务变成孤儿，
        但也不能无限重试——重试用尽后必须显式降级，而不是假装写成功。
        """
        attempts = max(1, int(self._settings.persist_max_attempts))
        for attempt in range(attempts):
            if not self._owns(task_id, execution_id):
                return PERSIST_SKIPPED
            try:
                self._repository.save(record)
                return PERSIST_PERSISTED
            except Exception:  # noqa: BLE001 - 任何落盘失败都先按可重试处理
                if attempt + 1 >= attempts:
                    return PERSIST_FAILED
                # 退避期间若本执行被取消，CancelledError 应当继续向上传播：
                # 那时终态的所有权已经转移（用户取消或调用方放弃会各自落盘），
                # 在这里报一个"落盘失败"只会制造虚假的降级信号。
                await asyncio.sleep(self._settings.persist_retry_backoff_seconds * (attempt + 1))
        return PERSIST_FAILED

    def _record_problem(self, task_id: str, execution_id: str, kind: str, detail: str) -> None:
        self._problems[task_id] = _ExecutionProblem(execution_id=execution_id, kind=kind, detail=detail)

    def _terminate_superseded_execution(self, task_id: str, execution_id: str) -> None:
        """终止**刚被取代的那一个**执行。

        必须比对 execution_id：如果在这中间已经有新执行接管，误杀它会让一个
        正常推进的任务凭空停住。
        """
        execution = self._executions.get(task_id)
        if execution is None or execution.execution_id != execution_id:
            return
        self._terminate_execution(execution)

    def _terminate_execution(self, execution: _Execution) -> None:
        if not execution.task.done():
            execution.task.cancel()

    def _abandon_execution(self, execution: _Execution, reason: str) -> None:
        """为被放弃的执行留下确定终态。

        这里是**同步**写入（`TaskRepository.save` 是同步的）：它会在协程已被取消时执行，
        此时任何 await 都可能立刻再次抛出 CancelledError。
        仍然校验执行身份——若已被用户取消或新执行接管，就不写。
        """
        record = self._owned(execution.task_id, execution.execution_id)
        if record is None:
            return
        # 已经有确定的终态时不得覆盖：调用方被取消与执行完成之间可能只差一个事件循环轮次，
        # 把已经落盘的 DRAFT_READY 改写成 FAILED 是比"不写"更糟的结果。
        if (record.get("status") or "") not in INTERRUPTED_STATUSES:
            return
        record["status"] = TaskStatus.FAILED.value
        record["error"] = reason
        events = list(record.get("events") or [])
        events.append(event("abandoned", reason))
        record["events"] = events
        try:
            self._repository.save(record)
        except Exception as error:  # noqa: BLE001 - 连放弃都写不进去时必须显式降级，不能沉默
            self._record_problem(
                execution.task_id,
                execution.execution_id,
                "persist",
                f"放弃的执行未能落盘：{type(error).__name__}: {error}",
            )

    def _begin_execution(self, task_id: str) -> str | None:
        """开启一次新执行：递增代际并分配 execution_id。

        代际的作用是**让上一个执行失去写权限**。取消与重跑都会走到这里，
        因此旧协程不可能把自己的结果覆盖到新状态上。
        """
        record = self._repository.get(task_id)
        if record is None:
            return None
        execution_id = uuid.uuid4().hex
        record["run_generation"] = int(record.get("run_generation") or 0) + 1
        record["execution_id"] = execution_id
        self._repository.save(record)
        # 新执行意味着重新开始：清掉上一个执行留下的降级记录。
        self._problems.pop(task_id, None)
        return execution_id

    def _persist_ledger(self, task_id: str, execution_id: str, payload: dict[str, Any]) -> None:
        """把调用账本落盘。

        只覆盖 `usage` 字段，避免把并发写入的其它状态（例如取消）盖回去；
        执行已被取代或取消时**直接抛错**，让调用停止——不允许产生没有账目的消耗。
        """
        record = self._repository.get(task_id)
        if record is None:
            raise LedgerPersistError(f"任务 {task_id} 不存在，调用账本无法落盘")
        if (record.get("execution_id") or "") != execution_id:
            raise LedgerPersistError("执行已被取消或被新执行接管：账本停止记录，不再继续调用模型")
        record["usage"] = payload
        self._repository.save(record)

    def _has_live_execution(self, task_id: str) -> bool:
        """该任务是否已有一次**尚未结束**的执行。"""
        execution = self._executions.get(task_id)
        return execution is not None and not execution.finished.is_set()

    def _owns(self, task_id: str, execution_id: str) -> bool:
        record = self._repository.get(task_id)
        return record is not None and (record.get("execution_id") or "") == execution_id

    def _owned(self, task_id: str, execution_id: str) -> dict[str, Any] | None:
        """取记录，并确认本次执行仍持有所有权。"""
        record = self._repository.get(task_id)
        if record is None or (record.get("execution_id") or "") != execution_id:
            return None
        return record

    def _save_if_owner(self, task_id: str, execution_id: str, record: dict[str, Any]) -> None:
        """单次、带所有权校验的落盘。

        取消与被接管都会换掉 execution_id，于是旧执行在这里被拒绝写入。
        注意这里**不是**吞掉持久化失败：真正落盘出错时 `save` 会照常抛出。
        需要"失败后重试"的场景用 `_persist_terminal`。
        """
        if self._owned(task_id, execution_id) is None:
            return
        self._repository.save(record)

    async def _execute(
        self, task_id: str, execution_id: str, resume: dict[str, Any] | None = None
    ) -> dict[str, Any] | None:
        """运行工作流并把结果整理成待落盘的记录。返回 None 表示不应落盘。

        这里**不**吞掉 CancelledError，也不负责释放句柄：取消的语义由发起方决定，
        资源的释放由 `_on_execution_done` 统一负责。这样"执行结束"与"结果已落盘"
        就不会像以前那样互相抢顺序。
        """
        record = self._owned(task_id, execution_id)
        if record is None:
            # 已被更新的执行接管，或任务已被取消：旧执行不得再写任何状态。
            return None
        try:
            record = await asyncio.wait_for(
                self._run_workflow(record, resume),
                timeout=self._settings.task_timeout_seconds,
            )
        except asyncio.TimeoutError:
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"任务超过 {self._settings.task_timeout_seconds:.0f} 秒未完成，已终止"
            record.setdefault("events", []).append(event("timeout", record["error"]))
        except asyncio.CancelledError:
            raise
        except Exception as error:  # noqa: BLE001 - 任何执行异常都要转成显式失败状态
            record["status"] = TaskStatus.FAILED.value
            record["error"] = f"执行失败：{type(error).__name__}: {error}"
            record.setdefault("events", []).append(event("failed", record["error"]))
        return record

    def _mark_interrupted_tasks(self) -> list[str]:
        """启动时处理上次进程遗留的在途任务。

        与 P0 的区别：**有检查点的任务标为可恢复**，不再一律"中断不续跑"。
        但这里**不自动续跑**——恢复必须由创建者显式发起，并在恢复前重新校验
        归属、代际与输入版本（见 `resume`）。留在 RECEIVED/RUNNING 会让健康检查与界面
        看起来还有任务在推进，而实际上没有任何协程在推动它，因此仍显式落一个终态。
        """
        interrupted: list[str] = []
        for record in self._repository.list():
            if record.get("status") not in INTERRUPTED_STATUSES:
                continue
            task_id = str(record.get("task_id") or "")
            resumable = bool(task_id) and has_checkpoint(self._settings, task_id)
            record["status"] = TaskStatus.FAILED.value
            record["awaiting_input"] = False
            if resumable:
                record["error"] = "进程重启：任务在执行中被中断，存在可恢复的检查点，需由创建者显式恢复"
                record["restart_policy"] = "checkpoint_available"
            else:
                record["error"] = "进程重启：任务在执行中被中断，且没有可用的检查点，无法续跑"
                record["restart_policy"] = "interrupted_without_resume"
            events = list(record.get("events") or [])
            events.append(event("restart", record["error"]))
            record["events"] = events
            self._repository.save(record)
            if task_id:
                interrupted.append(task_id)
        return interrupted

    async def _run_workflow(
        self, record: dict[str, Any], resume: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        """运行（或恢复）工作流。检查点按 task_id 作为线程。

        - `resume is None`：全新执行，先清掉该线程的旧检查点，避免意外"接着上次跑"；
        - `mode=interrupt`：从节点级中断继续，之前的节点不会重跑；
        - `mode=checkpoint`：续跑最后未完成的节点。

        **外部模型请求不保证 exactly-once，这里也不这么宣称**：一次模型调用可能在进程崩溃
        或请求被取消之前就已经发出去了，我们无法撤销它。能保证的是两点——
        (1) 每次真的发出过的请求都会按次记进调用账本（并在恢复/重启后续算），
        (2) 任务不会因为"看起来恢复过"就被当成没消耗过资源。因此恢复路径会显式写入
        `recovery_semantics=at_least_once`（见 `resume` / `clarify`），并把不确定性留在事件里。
        """
        task_id = record["task_id"]
        # dispatch 前已持久化的事件轨迹：检查点恢复时图返回的事件**不含** dispatch
        # 之前的增量（clarify 写入的 snapshot_selected/cleared 等），直接覆盖会丢审计；
        # 结束时用 _merge_event_lists 按最长公共前缀合并（审计只增不减）。
        pre_events = list(record.get("events") or [])
        initial_state: dict[str, Any] = {
            "task_id": task_id,
            "requirement": _effective_requirement(record),
            "slots": record.get("slots") or {},
            "schema_snapshot": record.get("schema_snapshot") or "",
            "input_version": record.get("input_version") or "",
            "events": list(record.get("events") or []),
            "revisions": 0,
            "max_revisions": self._settings.max_revisions,
        }

        # 任务级账本：provider 是共享的，消耗必须记在**每个任务自己的**账本上，
        # 否则并发任务会互相串账；预算上限也在这一层生效。
        execution_id = record.get("execution_id") or ""
        ledger = UsageLedger(
            max_total_tokens=int(getattr(self._settings, "max_task_tokens", 0) or 0),
            max_prompt_tokens=int(getattr(self._settings, "max_task_prompt_tokens", 0) or 0),
            max_cost_estimate=float(getattr(self._settings, "max_task_cost_estimate", 0.0) or 0.0),
            prompt_price_per_1k=float(getattr(self._settings, "llm_price_prompt_per_1k", 0.0) or 0.0),
            completion_price_per_1k=float(getattr(self._settings, "llm_price_completion_per_1k", 0.0) or 0.0),
            max_requests=int(getattr(self._settings, "max_task_requests", 0) or 0),
            # 未提供 usage 的请求按一个保守值计入预算判定；没配置就用单次输出上界。
            unknown_charge_tokens=int(getattr(self._settings, "unknown_usage_charge_tokens", 0) or 0)
            or int(self._settings.llm_max_tokens),
            task_id=task_id,
            execution_id=execution_id,
            model=self._settings.llm_model,
        )
        # 恢复、重试与重启都要**续算**：任务累计预算不得被清零。
        ledger.seed_from_prior(record.get("usage"))
        # 每次记账后落盘：恢复才有据可续，也确保不出现"没有账目的模型调用"。
        ledger.on_update = lambda payload: self._persist_ledger(task_id, execution_id, payload)

        try:
            async with open_checkpointer(self._settings) as saver:
                workflow = self._build_workflow(record, saver)
                with usage_scope(ledger, phase=PHASE_GENERATE):
                    if resume is None:
                        await _delete_thread(saver, task_id)
                        record["resume_mode"] = None
                        result = await workflow.run(initial_state, thread_id=task_id)  # type: ignore[arg-type]
                    elif resume.get("mode") == RESUME_INTERRUPT:
                        result = await workflow.resume_interrupt(thread_id=task_id, value=resume.get("value") or {})
                    else:
                        result = await workflow.continue_pending(thread_id=task_id)
        finally:
            # 成功、失败、被取消都要把账本落到本次执行的记录上：
            # 失败的执行同样消耗了模型调用，不能因此丢失账目（否则恢复会重置预算）。
            record["usage"] = ledger.as_dict()

        interrupts = result.get("__interrupt__") if isinstance(result, dict) else None
        if interrupts:
            # 图停在**节点级中断**上：状态是 NEEDS_INFO，但检查点可从此节点续跑。
            payload = _first_interrupt_value(interrupts)
            record["status"] = TaskStatus.NEEDS_INFO.value
            record["questions"] = payload.get("questions") or []
            record["error"] = None
            record["awaiting_input"] = True
            events = _merge_event_lists(pre_events, result.get("events") or record.get("events") or [])
            events.append(event("awaiting_input", "图停在节点级中断上，等待用户补充信息后从该节点继续"))
            record["events"] = events
            _annotate_recovery(record)
            return record

        record["awaiting_input"] = False
        investigation = result.get("investigation") or {}
        record.update(
            {
                "status": result.get("status", TaskStatus.FAILED.value),
                "questions": result.get("questions") or [],
                "draft": result.get("draft"),
                "events": _merge_event_lists(pre_events, result.get("events") or record.get("events") or []),
                "revisions": int(result.get("revisions") or 0),
                "error": result.get("error"),
                "evidence_note": result.get("evidence_note"),
                # 调查轨迹与预算原样落库，供工作台展示"实际执行了什么、为什么停下"。
                "investigation": investigation or record.get("investigation"),
                "strategy": investigation.get("strategy") or record.get("strategy"),
            }
        )
        # 草案（重新）生成后保留一份版本快照；内容未变时不重复追加。
        _record_draft_version(record, origin="agent", actor="agent", reason="工作流生成", created_at=datetime.now(timezone.utc))
        # 草案重新生成后，与当前内容不一致的旧确认立即失效（保留痕跡）。
        _invalidate_stale_confirmations(record, "草案已重新生成，旧确认失效")
        _annotate_recovery(record)
        return record

    def _retriever_for(self, organization_id: str, application_id: str = "") -> Any:
        """按 (组织, 应用) 构建检索器：公开合成语料 + 该组织可见的导入知识。

        权限过滤在**服务端**这一层完成：只有通过 `is_visible`（组织匹配、应用匹配、
        处于生效状态）的知识才会被加入检索器，模型无法用参数扩大范围。
        """
        retriever = build_retriever(self._settings.demo_dir)
        extra = visible_chunks(self._knowledge.list(), organization_id, application_id)
        if extra:
            retriever.add(extra)
        return retriever

    def _build_workflow(self, record: dict[str, Any], checkpointer: Any) -> DraftWorkflow:
        # 检索只用**已核对通过**的应用；请求里声明的应用不能直接决定可见范围。
        retriever = self._retriever_for(
            str(record.get("organization_id") or ""), str(record.get("authorized_application") or "")
        )
        toolbox_factory = lambda snapshot: Toolbox(  # noqa: E731 - 需要按任务注入快照
            settings=self._settings,
            retriever=retriever,
            schema_snapshot=snapshot,
        )
        return DraftWorkflow(
            WorkflowDeps(
                settings=self._settings,
                provider=self._provider,
                trusted_context=TrustedContext(
                    user_id=record.get("user_id") or "",
                    organization_id=record.get("organization_id") or "",
                ),
                toolbox_factory=toolbox_factory,
                checkpointer=checkpointer,
            )
        )

    async def _inspect_checkpoint(self, task_id: str) -> dict[str, Any] | None:
        """读取检查点：是否有待恢复的工作、是否停在中断上、记录在案的输入版本。"""
        async with open_checkpointer(self._settings) as saver:
            workflow = self._build_workflow(self._require(task_id), saver)
            state = await workflow.snapshot(thread_id=task_id)
        values = getattr(state, "values", None) or {}
        next_nodes = list(getattr(state, "next", ()) or ())
        if not values and not next_nodes:
            return None  # 该线程没有检查点
        pending_interrupt = any(
            getattr(task, "interrupts", ()) for task in (getattr(state, "tasks", ()) or ())
        )
        return {
            "next": next_nodes,
            "interrupt": bool(pending_interrupt),
            "input_version": str(values.get("input_version") or ""),
        }
