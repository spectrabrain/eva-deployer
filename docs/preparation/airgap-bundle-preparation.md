# EVA Airgap Bundle Preparation

> 작성 중

이 문서는 Local Repository 설치에 사용할 검증된 Airgap Bundle 생성 절차를 제공할 예정입니다.

대상 범위:

- Release 및 version 검증
- Chart, Runtime 및 OS package 다운로드
- Container image 다운로드와 archive 생성
- EVA Agent 및 vLLM model cache 준비
- Qdrant snapshot 준비
- Bundle manifest와 무결성 검증
- 폐쇄망 반입 전 최종 확인

Target 설치와 Local Harbor 구성은 [Local Repository Runbook](../installation/local-repository-runbook.md)을 사용합니다.
