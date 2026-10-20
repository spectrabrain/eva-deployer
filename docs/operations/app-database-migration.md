# App DB 보존·라이선스 identity 정합화와 이전 사용자 백업

이 기능이 포함된 **Deployer Release의 App role**을 실행할 때 적용된다. Helm 직접 설치나 기존
Release에는 적용되지 않는다. Cloud/Remote/Local 모두 같은 절차이며 인터넷 연결은 필요 없다.
DB 복사, 라이선스 identity 정합화, 제품의 Alembic schema upgrade는 서로 다른 단계다.
**정상적인 `eva_v3_0`이 이미 있으면 삭제하거나 이전 DB로 덮어쓰지 않고 그대로 사용한다.**

## 기본 동작

1. Helm server dry-run과 기존 소유권 검증 후 App을 0 replica로 내리고 Pod 종료를 기다린다.
2. **CLI `--set`까지 적용한 실제 Chart 렌더**에서 대상 DB 이름을 읽는다. Chart 기본값이
   `eva_v3_0`이면 해당 이름을 사용하고, 명시한 `database.name`이 있으면 그 이름을 사용한다.
3. 기존 내부 MySQL의 준비 상태·Pod 소유 관계·연결값·저장소와 App 데이터/license mount가
   유지되는지 검사한다. 비밀번호를 새로 설정하거나 라이선스 서버에 요청하지 않는다.
4. 대상 DB에 `config`와 `alembic_version` 테이블이 이미 있으면 **그 대상 DB 데이터를 유지**한다.
   남아 있는 `eva`의 ID가 다르다는 이유만으로 중단하거나 그 데이터를 합치지 않는다.
   대상이 비었을 때만 기존 연결 DB/legacy `eva` 또는 명시적 source에서 복사할 수 있다.
5. 기존 App 이미지와 license 볼륨만 사용하는 임시 Pod에서 암호화된 로컬 license cache를
   **읽기 전용으로** 읽는다. App/LicenseManager 초기화·DB 접속·라이선스 서버 통신·활성화·cache
   변경을 하지 않는다. license cache의 product와 `eva_id:` hardware identity를 검증한다.
6. 사용할 DB의 `user.user_password` 열이 있으면 **DB 이름 변경 여부와 관계없이** 사용자 전체 CSV를
   먼저 기록한다. `keycloak_user_id`만 있는 IAM schema는 백업 대상이 아니다. 알 수 없는
   사용자 schema는 추측하지 않고 설치를 중단한다. Alembic revision 문자열의 대소 비교로
   제품 버전을 판정하지 않는다.
7. DB 복사가 필요하면 원본의 SQL dump를 만든 뒤 **테이블이 없는 대상 DB에만** 복원한다.
   원본/대상의 테이블 목록·실제 행 수·Alembic revision·`config.eva_id`와 canonical dump의
   SHA-256을 비교한다. 이 복사 단계는 원본 ID를 그대로 보존한다.
8. 로컬 cache의 ID와 대상 DB ID가 같으면 그대로 진행한다. 다르면 **대상 DB 전체 dump,
   원본 암호화 cache, 변경 전 identity 기록을 영구 private 백업한 후 `config.eva_id` 한 행만**
   compare-and-set으로 수정하고 검증한다. 다른 설정·사용자·카메라·바이너리 데이터·legacy DB는
   이 정합화 단계에서 변경하지 않는다. cache가 없으면 DB ID를 추측/생성/수정하지 않는다.
9. 검증 성공 후에만 새 App을 기동한다. App 이미지의 기존 prestart가 Alembic upgrade를
   수행한다. v3.2의 사용자 테이블 재생성보다 CSV/SQL 백업이 먼저 완료된다.

**원본 DB를 자동으로 삭제하지 않는다.** 성공하더라도 백업과 원본을 보존한다. 삭제는 실제
로그인·데이터·license·IAM 사용자 재등록 검증과 보관 정책 승인 후 별도로 수행한다.

App이 중단되는 유지보수 작업이다. 별도 writer·HPA·GitOps가 App을 재기동하거나 DB를 수정하지
않게 하고, DB·App 데이터·license의 별도 복구 가능한 백업도 먼저 준비한다. helper는 원본
license 디렉터리를 변경하지 않으며 ID 보정 시 암호화 cache 파일의 복사본만 백업한다.
DB 이름/ID를 바꾸는 것만으로 IAM 사용자가 생성되지는 않는다. **다른 데이터 보존은 이 helper의
정합화 범위에 대한 보장**이다. 이후 App 자체 schema/data migration이 수행하는 변경까지
생략하거나 차단한다는 의미는 아니다.

로컬 cache는 기존에 저장된 identity의 근거이며 **라이선스 서버의 현재 활성 device·구독·권한을
확인한 결과가 아니다.** ID 정합화 후에도 서버 측 등록 문제는 별도 점검이 필요할 수 있다.

## 백업 위치와 CSV 취급

기본 위치는 **Ansible 실행 대상 서버**의
`/var/lib/eva/backups/app-db/<site>/<release>/<UTC시각>-<고유값>/`이다.
Bastion controller로 자동 복사하지 않는다. 설치 로그의 `Show private EVA App DB backup location`
메시지에서 실제 경로를 확인한다. 이 위치는 버전별 App workdir 재생성으로 삭제되지 않는다.

| 파일 | 내용 |
| --- | --- |
| `user_accounts_all.csv` | 로컬 사용자 테이블의 모든 열, 열 이름 header, UTF-8 CSV |
| `user_accounts_all.metadata.json` | source DB, 열·행 수, checksum, NULL 규칙, 민감정보 안내 |
| `<source>.sql` | 이전/사용자 schema upgrade 전 DB 전체 dump |
| `source.json` | 복사 전 DB 검증 정보와 dump checksum; DB 이름 이전 시 생성 |
| `<target>-verified.sql` | 복원 DB의 비교용 dump; DB 이름 이전 시 생성 |
| `copy-complete.json` | DB 복사 검증 성공 기록; App 정상 동작 보증은 아님 |
| `<target>-before-identity.sql` | ID 보정 직전 대상 DB 전체 dump |
| `license-cache-before-identity.key` | ID 보정의 근거인 암호화 cache 원본 복사본 |
| `identity-before.json` | 이전/보정 ID의 hex, cache·dump checksum, 변경 전 검증 정보; 민감정보 |
| `identity-complete.json` | `config.eva_id` 단독 변경 검증 성공 기록 |
| `result.json` | 완료된 DB 준비 결과와 CSV 경로 |

디렉터리는 `0700`, 파일은 `0600`이며 실행 계정 소유다. 매 실행마다 새 디렉터리를 사용하고
이전 백업은 덮어쓰지 않는다. 실패하면 부분 파일이 남을 수 있으므로 완료 metadata/checksum을
확인한다. SQL NULL은 CSV에 `\N`, 빈 문자열은 빈 필드로 기록한다. 쉼표·따옴표·줄바꿈·탭·한글을
보존한다. **CSV에는 비밀번호 hash와 개인정보도 포함**되므로 커밋·공유 로그 업로드를 금지한다.
암호화 cache·SQL·identity journal도 민감정보다. 일반 로그/지원 티켓에 첨부하지 않는다.
CSV는 원문 아카이브이지 IAM 자동 import 파일이 아니다. 스프레드시트가 수식을 실행하지 않게
텍스트로 열고, 설치자는 필요한 계정을 확인해 EVA IAM에서 새로 등록하고 비밀번호를 발급한다.

## 옵션

단일 설치 설정의 `targets.<alias>.ansible` 또는 기존 Inventory의 target 변수로 지정한다.
Chart Values 아래에 넣는 옵션이 아니다.

| 변수 | 기본값 | 의미 |
| --- | --- | --- |
| `eva_app_db_auto_migrate` | `true` | 비어 있는 대상으로 DB dump/restore 허용. `false`라도 사용자 CSV 백업과 기존 대상 ID 정합화는 유지; 복사가 필요하면 중단 |
| `eva_app_db_migration_source` | 빈 값 | 비어 있는 대상의 복사 원본을 명시. 기본은 기존 연결 DB, 필요 시 legacy `eva`; 대상이 이미 있으면 동일한 이름만 허용 |
| `eva_app_db_backup_root` | 위 Site/release별 경로 | 대상 서버의 private 절대 경로. symlink 경로는 거부 |

기존 `eva`를 유지하려면 `targets.<alias>.values.app.database.name: eva`를 명시한다.
반대로 기본 Chart DB로 이전하려면 이 override를 없애거나 원하는 DB 이름을 명시한다.
`eva_app_db_migration_source`는 덮어쓰기 승인 옵션이 아니다. populated 대상과 다른 source를
명시하면 요청을 무시해 진행하는 대신 중단한다. 일반적인 기존 대상 유지에는 이 옵션이 필요 없다.

## 읽기 전용 license reader의 실행 조건

- 기존 App의 license mount와 이미지, product code, 파일 암호화 설정을 사용한다. product나
   파일 암호화 설정 변경은 자동 identity 보정과 섞지 않고 수동 검토한다.
- App namespace에 임시 Pod/NetworkPolicy 생성·조회·대기·삭제와 Pod exec 권한이 필요하다.
   reader는 DB 비밀번호/API key를 전달받지 않으며 service-account token을 mount하지 않는다.
   root filesystem과 license mount는 읽기 전용이며 ingress/egress 차단 policy를 사용한다.
- PVC는 읽기 전용으로 mount한다. hostPath는 잘못된 노드의 cache를 읽지 않도록 **검증된 단일
   노드 cluster에서만** 자동 지원한다. 노드 조회 권한도 필요하다. 다중 노드 hostPath는 수동 검토한다.
- hostPath license 디렉터리가 없으면 자동 생성하지 않는다. 이미지 pull/mount/권한 오류는
   cache 파일이 없는 경우와 다르며 설치를 중단한다. mount는 정상이고 `License.key`만 없으면
   `no-local-license`로 진행하되 ID는 바꾸지 않는다.
- 작업 후 임시 Pod와 NetworkPolicy를 제거한다. 정리 실패 시 새 App 설치 전에 중단한다.
   `eva-license-reader-*` 리소스가 남았으면 해당 작업의 리소스인지 검토한 후 정리한다.

## 자동 처리를 중단하는 경우

- populated 대상에 필수 EVA 테이블/identity가 없거나 최신 복사 기록에 성공 marker가 없음:
   이전 실패의 부분 복사본을 정상 DB로 취급하지 않는다. **자동 DROP/merge/덮어쓰기를 하지 않는다.**
- cache 손상·product 불일치·지원하지 않는 hardware identity·읽기 실패: legacy DB ID를 대신
   사용하지 않는다. cache와 DB를 검토한다. 정상 cache의 ID가 DB와 다른 것 자체는 중단 사유가 아니다.
- ID 보정 대상 `config`가 비-InnoDB이거나 trigger 또는 `config.value`를 참조하는 FK가 있음:
   다른 데이터가 함께 변경될 수 있으므로 자동 보정을 하지 않는다. 백업·검증·CAS 실패도 중단한다.
- dump/restore/검증 실패: dump와 부분 대상 DB를 보존하고 **새 App 설치 전에 실패**한다.
  이전 Deployment UID가 같으면 기존 replica 복원을 시도한다. 원본으로 복귀한 App이 쓴 데이터와
  부분 대상 DB를 확인해야 하므로 실패한 복사를 자동 재개/덮어쓰기하지 않는다.
- 외부 DB, root가 아닌 기존 App DB 사용자, DB endpoint/credential/저장소 변경: 지원 범위 밖이므로
  사전 수동 migration/사용자 CSV 백업과 검토가 필요하다. 보호 절차를 끄고 진행하는 옵션은 없다.
- 비-InnoDB 테이블, view/routine/event/trigger, cross-DB FK: schema-qualified 참조를 잘못 복사하지
  않도록 자동 이름 이전을 거부한다. 단순 DB dump 명령의 성공만으로 안전하다고 판단하지 않는다.
- 준비되지 않은 MySQL, 남아 있는 App Pod, 모호한 MySQL controller: 재시작으로 우회하지 않는다.

schema migration 이후 Helm rollback만으로 DB/user 테이블이 복구되지 않는다. CSV는 사용자
수동 재등록에 쓰고, DB 전체 복구가 필요하면 검증한 SQL 백업과 제품의 복구 절차를 따른다.

## 기존 `eva`와 `eva_v3_0`의 ID가 다른 경우

예를 들어 `eva_v3_0`에 유지할 데이터가 있고 기존 license cache가 다른 ID를 가리켜도,
이 수정본은 대상 DB를 비우거나 `eva` 데이터를 덮어쓰지 않는다. `eva_v3_0`의 전체 백업을
보관하고 cache에 저장된 ID로 `eva_v3_0.config.eva_id`만 보정한 다음 설치를 계속한다.
남아 있는 `eva`와 license cache는 변경하지 않는다. 따라서 정상 대상 DB를 삭제할 필요가 없다.

cache와 대상 ID가 이미 같으면 ID 수정 없이 진행한다. cache가 없으면 남아 있는 `eva`의 ID를
대체 근거로 삼지 않는다. 로그 결과의 `license_identity`는 `corrected` / `matched` /
`no-local-license`이며 실제 ID와 복호화된 license 내용은 일반 로그에 출력하지 않는다.

이 동작은 **수정된 App role이 포함된 새 Deployer Release**에서 적용된다. 기존 `ci.125`는
계속 이전 충돌 중단 동작을 한다. 그 Release에 `from_json` 표시 오류만 고치는 것으로는
이 정책이 적용되지 않는다. 수정본은 helper의 `rc == 0`일 때만 결과 JSON을 파싱한다.
실패 시 기존 Deployment UID가 같으면 이전 replica 복구를 시도하지만 이는 DB ID 보정이나
제품 migration을 되돌리는 rollback이 아니다. `replica restoration rc=0, rollout rc=0`도
로그인·license 정상 동작을 보증하지 않는다.

설정도 변경했다면 원본을 다시 읽는 새 `eva install --config ...` 작업을 시작한다.
`eva retry`는 이전 Workspace snapshot을 사용하므로 수정한 원본 입력을 반영하지 않는다.
