# App DB 이름 이전과 v3.2 이전 사용자 백업

이 기능이 포함된 **Deployer Release의 App role**을 실행할 때 적용된다. Helm 직접 설치나 기존
Release에는 적용되지 않는다. Cloud/Remote/Local 모두 같은 절차이며 인터넷 연결은 필요 없다.
DB 복사와 제품의 Alembic schema upgrade는 서로 다른 단계다.

## 기본 동작

1. Helm server dry-run과 기존 소유권 검증 후 App을 0 replica로 내리고 Pod 종료를 기다린다.
2. **CLI `--set`까지 적용한 실제 Chart 렌더**에서 대상 DB 이름을 읽는다. Chart 기본값이
   `eva_v3_0`이면 해당 이름을 사용하고, 명시한 `database.name`이 있으면 그 이름을 사용한다.
3. 기존 내부 MySQL의 준비 상태·Pod 소유 관계·연결값·저장소와 App 데이터/license mount가
   유지되는지 검사한다. 비밀번호를 새로 설정하거나 라이선스 서버에 요청하지 않는다.
4. 기존 `user.user_password` 열이 있으면 **DB 이름 변경 여부와 관계없이** 사용자 전체 CSV를
   먼저 기록한다. `keycloak_user_id`만 있는 IAM schema는 백업 대상이 아니다. 알 수 없는
   사용자 schema는 추측하지 않고 설치를 중단한다. Alembic revision 문자열의 대소 비교로
   제품 버전을 판정하지 않는다.
5. DB 이름이 바뀌면 원본의 SQL dump를 만든 뒤 **테이블이 없는 대상 DB에만** 복원한다.
   원본/대상의 테이블 목록·실제 행 수·Alembic revision·`config.eva_id`와 canonical dump의
   SHA-256을 비교한다. `eva_id`를 생성/수정하지 않고 원본 그대로 보존한다.
6. 검증 성공 후에만 새 App을 기동한다. App 이미지의 기존 prestart가 Alembic upgrade를
   수행한다. v3.2의 사용자 테이블 재생성보다 CSV/SQL 백업이 먼저 완료된다.

**원본 DB를 자동으로 삭제하지 않는다.** 성공하더라도 백업과 원본을 보존한다. 삭제는 실제
로그인·데이터·license·IAM 사용자 재등록 검증과 보관 정책 승인 후 별도로 수행한다.

App이 중단되는 유지보수 작업이다. 별도 writer·HPA·GitOps가 App을 재기동하거나 DB를 수정하지
않게 하고, DB·App 데이터·license의 별도 복구 가능한 백업도 먼저 준비한다. 복사 helper는
license 디렉터리 자체를 변경/백업하지 않으며 DB 이름을 바꾸는 것만으로 IAM 사용자가 생성되지 않는다.

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
| `result.json` | 완료된 DB 준비 결과와 CSV 경로 |

디렉터리는 `0700`, 파일은 `0600`이며 실행 계정 소유다. 매 실행마다 새 디렉터리를 사용하고
이전 백업은 덮어쓰지 않는다. 실패하면 부분 파일이 남을 수 있으므로 완료 metadata/checksum을
확인한다. SQL NULL은 CSV에 `\N`, 빈 문자열은 빈 필드로 기록한다. 쉼표·따옴표·줄바꿈·탭·한글을
보존한다. **CSV에는 비밀번호 hash와 개인정보도 포함**되므로 커밋·공유 로그 업로드를 금지한다.
CSV는 원문 아카이브이지 IAM 자동 import 파일이 아니다. 스프레드시트가 수식을 실행하지 않게
텍스트로 열고, 설치자는 필요한 계정을 확인해 EVA IAM에서 새로 등록하고 비밀번호를 발급한다.

## 옵션

단일 설치 설정의 `targets.<alias>.ansible` 또는 기존 Inventory의 target 변수로 지정한다.
Chart Values 아래에 넣는 옵션이 아니다.

| 변수 | 기본값 | 의미 |
| --- | --- | --- |
| `eva_app_db_auto_migrate` | `true` | DB 이름 변경의 dump/restore 허용. `false`라도 필요한 사용자 CSV 백업은 수행하고, 이름이 달라 복사가 필요하면 중단 |
| `eva_app_db_migration_source` | 빈 값 | 기존 App 연결 DB에서 자동 결정. 복구 검토 후 다른 원본 DB를 명시할 때 사용 |
| `eva_app_db_backup_root` | 위 Site/release별 경로 | 대상 서버의 private 절대 경로. symlink 경로는 거부 |

기존 `eva`를 유지하려면 `targets.<alias>.values.app.database.name: eva`를 명시한다.
반대로 기본 Chart DB로 이전하려면 이 override를 없애거나 원하는 DB 이름을 명시한다.
`eva_app_db_migration_source`는 덮어쓰기 승인 옵션이 아니며 populated 대상은 항상 거부한다.

## 자동 처리를 중단하는 경우

- 대상 DB에 테이블이 이미 있음: **자동 DROP/merge/덮어쓰기를 하지 않는다.** 원본은 보존된다.
- 이전 설치가 이미 `eva_v3_0`을 만들었고, 남아 있는 `eva`와 `eva_id`가 다름: 라이선스 identity를
  임의로 고르지 않는다. 두 DB·license를 모두 백업하고 정본을 결정해 수동 복구한다.
  의도적으로 현재 DB를 유지하기로 결정했다면 그 이름을 source로 명시할 수 있으나,
  이 설정이 잘못된 license identity를 복구하거나 라이선스를 재활성화하지는 않는다.
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
