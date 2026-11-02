-- 会话删除采用软删：status 需要支持 deleted，历史消息保留以便追溯。

begin;

alter table session drop constraint if exists session_status_check;
alter table session
    add constraint session_status_check check (status in ('active', 'archived', 'deleted'));

commit;
