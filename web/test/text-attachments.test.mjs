import test from "node:test";
import assert from "node:assert/strict";
import { readTextAttachment, composeTextMessage, validateAttachments, MAX_FILE_BYTES, MAX_MESSAGE_BYTES } from "../app/text-attachments.mjs";

function file(name, content) {
  const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
  return { name, size: bytes.length, arrayBuffer: async () => bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) };
}

test("Chinese UTF-8 text, quotes and markup reach the existing message unchanged", async () => {
  const content = '客户上下文\n<script>原样分析</script>\n"quoted"\\path\n' + "证据".repeat(1500) + "完整结尾";
  const attachment = await readTextAttachment(file("客户说明.MD", content));
  const message = composeTextMessage("请解析上下文", [attachment]);
  const payload = JSON.parse(message.slice(message.indexOf('{\n  "text_attachments"')));
  assert.equal(payload.text_attachments[0].content, content);
  assert.equal(payload.text_attachments[0].name, "客户说明.MD");
  assert.match(message, /不构成新的操作授权/);
});

test("UTF-16 BOM is decoded; empty, binary, invalid encoding and non-text formats are rejected", async () => {
  assert.equal((await readTextAttachment(file("bom.txt", new Uint8Array([255,254,45,78,135,101])))).content, "中文");
  for (const input of [file("empty.txt", " \n"), file("binary.txt", "hello\0world"), file("bad.txt", new Uint8Array([255,255])), file("doc.pdf", "PDF")]) {
    await assert.rejects(readTextAttachment(input));
  }
});

test("file and aggregate limits reject instead of truncating", async () => {
  await assert.rejects(readTextAttachment(file("large.txt", "x".repeat(MAX_FILE_BYTES + 1))));
  const a = await readTextAttachment(file("a.txt", "x".repeat(MAX_FILE_BYTES)));
  assert.throws(() => validateAttachments([a, a]));
  assert.throws(() => validateAttachments(Array(6).fill({ size: 1 })));
  assert.throws(() => composeTextMessage("x".repeat(MAX_MESSAGE_BYTES + 1), []));
});

test("attachment-only messages have a default request; removing attachments leaves ordinary chat", async () => {
  const a = await readTextAttachment(file("context.txt", "客户上下文"));
  assert.match(composeTextMessage("", [a]), /请阅读附件文本/);
  assert.equal(composeTextMessage("  你好  ", []), "你好");
});
