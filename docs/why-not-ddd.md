# why not ...

pats is not the only tool that runs coding agents on tasks and scores what they do. this page goes through the ones
we found and says how each compares. sometimes that is a real difference, and sometimes the honest answer is that
the other tool does the job, and if you like it, use it.

## index

- [harbor](#harbor) https://github.com/harbor-framework/harbor
- [uipath coder_eval](#uipath-coder_eval) https://github.com/UiPath/coder_eval
- [benchflow and skillsbench](#benchflow-and-skillsbench) https://github.com/benchflow-ai/benchflow
- [tessl evals](#tessl-evals) https://docs.tessl.io/improving-your-skills/evaluate-skill-quality-using-scenarios.md
- [mlflow genai eval](#mlflow-genai-eval) https://github.com/mlflow/mlflow
- [nvidia openshell](#nvidia-openshell) https://github.com/NVIDIA/OpenShell
- [docker sandboxes](#docker-sandboxes) https://docs.docker.com/ai/sandboxes/
- [jfrog agent-belt](#jfrog-agent-belt) https://github.com/jfrog/agent-belt
- [agent-skill-eval](#agent-skill-eval) https://github.com/tardigrde/agent-skill-eval
- [agentbench-live](#agentbench-live) https://github.com/jackjin1997/AgentBench-Live
- [agentbox-sdk](#agentbox-sdk) https://github.com/TwillAI/agentbox-sdk
- [aider benchmark harness](#aider-benchmark-harness) https://github.com/Aider-AI/aider/tree/main/benchmark
- [braintrust](#braintrust) https://www.braintrust.dev
- [claude plugin eval](#claude-plugin-eval) https://code.claude.com/docs/en/plugin-evals
- [cli arena](#cli-arena) https://pypi.org/project/cli-arena/
- [coding.sh](#codingsh) https://github.com/jackulau/coding.sh
- [daytona](#daytona) https://github.com/daytonaio/daytona
- [e2b](#e2b) https://github.com/e2b-dev/E2B
- [enclave](#enclave) https://github.com/eclipse-enclave/enclave
- [evalscope](#evalscope) https://github.com/modelscope/evalscope
- [hal-harness](#hal-harness) https://github.com/princeton-pli/hal-harness
- [harness arena](#harness-arena) https://github.com/Ondemand-OSS/harness-arena
- [harness-bench](#harness-bench) https://github.com/Aurelien7877/harness-bench
- [inspect ai and inspect_swe](#inspect-ai-and-inspect_swe) https://github.com/UKGovernmentBEIS/inspect_ai
- [kbench](#kbench) https://github.com/shareAI-lab/kbench
- [langsmith](#langsmith) https://docs.langchain.com/langsmith/evaluation-concepts
- [modal sandboxes](#modal-sandboxes) https://modal.com/docs/guide/sandbox
- [nasde-toolkit](#nasde-toolkit) https://github.com/NoesisVision/nasde-toolkit
- [openbench](#openbench) https://github.com/minghinmatthewlam/openbench
- [openhands benchmarks](#openhands-benchmarks) https://github.com/OpenHands/benchmarks
- [arize phoenix](#arize-phoenix) https://github.com/Arize-ai/phoenix
- [promptfoo](#promptfoo) https://github.com/promptfoo/promptfoo
- [skill lab](#skill-lab) https://github.com/8ddieHu0314/Skill-Lab
- [skillgrade](#skillgrade) https://github.com/mgechev/skillgrade
- [skilled](#skilled) https://github.com/DeadHead04/skillEd
- [supabase evals](#supabase-evals) https://github.com/supabase/evals
- [swe-bench](#swe-bench) https://github.com/SWE-bench/SWE-bench
- [tracetrial-bench](#tracetrial-bench) https://github.com/TraceLite-AI/TraceTrial-Bench
- [valkyrie](#valkyrie) https://github.com/vals-ai/valkyrie
- [vercel agent-eval](#vercel-agent-eval) https://github.com/vercel-labs/agent-eval
- [vibekit](#vibekit) https://github.com/superagent-ai/vibekit
- [vivaria](#vivaria) https://github.com/METR/vivaria
- [waza](#waza) https://github.com/microsoft/waza
- [agentevals](#agentevals) https://github.com/langchain-ai/agentevals
- [canonical model-compare](#canonical-model-compare) https://github.com/canonical/model-compare
- [deepeval](#deepeval) https://github.com/confident-ai/deepeval
- [dev-fleet evals](#dev-fleet-evals) https://github.com/dev-fleet/evals
- [gemini-cli evals](#gemini-cli-evals) https://github.com/google-gemini/gemini-cli/tree/main/evals
- [langfuse](#langfuse) https://github.com/langfuse/langfuse
- [openai evals](#openai-evals) https://github.com/openai/evals
- [opencode-bench](#opencode-bench) https://github.com/anomalyco/opencode-bench
- [runloop](#runloop) https://docs.runloop.ai
- [skill-harness](#skill-harness) https://github.com/MrBinnacle/skill-harness
- [skillci](#skillci) https://github.com/kabirnarang39/skillci
- [tau-bench](#tau-bench) https://github.com/sierra-research/tau-bench

## harbor

https://github.com/harbor-framework/harbor

tbh, harbor is great and it kind of does the job. it runs real agent clis on tasks in a fresh sandbox per trial, scores them with a test script or an llm / agent judge. if you like it, use it.

you might want to choose pats instead if you want lxd (container or vm, remote or local) and bwrap drivers, for linux boxes and ci runners without docker.

also pats has a more sophisticated egress proxy (if anyone from harbor comes across this, pls ff to use / get inspired! :D). harbor has only allowlists, so a task that needs a host open but one repo or path on it closed has no harbor form. there is no per-request log either, so nothing tells you whether the agent fetched the answer.

anoter difference is that harbor tasks are self-contained packages with no shared defaults and no host-side steps. that means they are very isolated but also can be cumbersome and one tends to need a generator file.

pats is by-design poor in terms of the output formatting -- it expects ppl to do the parsing of the .pats ouptut themselves. having said that `harbor view` *is* really nice...

## uipath coder_eval

https://github.com/UiPath/coder_eval

...

## benchflow and skillsbench

https://github.com/benchflow-ai/benchflow

...

## tessl evals

https://docs.tessl.io/improving-your-skills/evaluate-skill-quality-using-scenarios.md

...

## mlflow genai eval

https://github.com/mlflow/mlflow

...

## nvidia openshell

https://github.com/NVIDIA/OpenShell

...

## docker sandboxes

https://docs.docker.com/ai/sandboxes/

...

## jfrog agent-belt

https://github.com/jfrog/agent-belt

...

## agent-skill-eval

https://github.com/tardigrde/agent-skill-eval

...

## agentbench-live

https://github.com/jackjin1997/AgentBench-Live

...

## agentbox-sdk

https://github.com/TwillAI/agentbox-sdk

...

## aider benchmark harness

https://github.com/Aider-AI/aider/tree/main/benchmark

...

## braintrust

https://www.braintrust.dev

...

## claude plugin eval

https://code.claude.com/docs/en/plugin-evals

...

## cli arena

https://pypi.org/project/cli-arena/

...

## coding.sh

https://github.com/jackulau/coding.sh

...

## daytona

https://github.com/daytonaio/daytona

...

## e2b

https://github.com/e2b-dev/E2B

...

## enclave

https://github.com/eclipse-enclave/enclave

...

## evalscope

https://github.com/modelscope/evalscope

...

## hal-harness

https://github.com/princeton-pli/hal-harness

...

## harness arena

https://github.com/Ondemand-OSS/harness-arena

...

## harness-bench

https://github.com/Aurelien7877/harness-bench

...

## inspect ai and inspect_swe

https://github.com/UKGovernmentBEIS/inspect_ai

...

## kbench

https://github.com/shareAI-lab/kbench

...

## langsmith

https://docs.langchain.com/langsmith/evaluation-concepts

...

## modal sandboxes

https://modal.com/docs/guide/sandbox

...

## nasde-toolkit

https://github.com/NoesisVision/nasde-toolkit

...

## openbench

https://github.com/minghinmatthewlam/openbench

...

## openhands benchmarks

https://github.com/OpenHands/benchmarks

...

## arize phoenix

https://github.com/Arize-ai/phoenix

...

## promptfoo

https://github.com/promptfoo/promptfoo

...

## skill lab

https://github.com/8ddieHu0314/Skill-Lab

...

## skillgrade

https://github.com/mgechev/skillgrade

...

## skilled

https://github.com/DeadHead04/skillEd

...

## supabase evals

https://github.com/supabase/evals

...

## swe-bench

https://github.com/SWE-bench/SWE-bench

...

## tracetrial-bench

https://github.com/TraceLite-AI/TraceTrial-Bench

...

## valkyrie

https://github.com/vals-ai/valkyrie

...

## vercel agent-eval

https://github.com/vercel-labs/agent-eval

...

## vibekit

https://github.com/superagent-ai/vibekit

...

## vivaria

https://github.com/METR/vivaria

...

## waza

https://github.com/microsoft/waza

...

## agentevals

https://github.com/langchain-ai/agentevals

...

## canonical model-compare

https://github.com/canonical/model-compare

...

## deepeval

https://github.com/confident-ai/deepeval

...

## dev-fleet evals

https://github.com/dev-fleet/evals

...

## gemini-cli evals

https://github.com/google-gemini/gemini-cli/tree/main/evals

...

## langfuse

https://github.com/langfuse/langfuse

...

## openai evals

https://github.com/openai/evals

...

## opencode-bench

https://github.com/anomalyco/opencode-bench

...

## runloop

https://docs.runloop.ai

...

## skill-harness

https://github.com/MrBinnacle/skill-harness

...

## skillci

https://github.com/kabirnarang39/skillci

...

## tau-bench

https://github.com/sierra-research/tau-bench

...
