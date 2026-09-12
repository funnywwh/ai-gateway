-- M34：可交互预览的握手凭证。
--
-- 它必须落库而不是每次服务时现生成：控制台要持有同一个值才能校验页面的握手，而控制台
-- **读不到**沙箱 iframe 的文档（省略 allow-same-origin 的文档是不透明源，父窗口看到的
-- contentDocument 是 null）。所以凭证由控制台生成、随上传提交、服务端注入进页面，
-- 两端各自留一份。
--
-- 空串 = 这份预览不可交互（历史行默认如此，行为与 M32 的只读预览一致）。
ALTER TABLE chat_artifacts ADD COLUMN bridge_token TEXT NOT NULL DEFAULT '';
