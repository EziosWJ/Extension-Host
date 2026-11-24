const callButton = document.querySelector("#call");
const result = document.querySelector("#result");

callButton.addEventListener("click", async () => {
  callButton.disabled = true;
  result.textContent = "正在调用…";
  try {
    const response = await fetch("../api/hello");
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || response.statusText);
    result.textContent = data.message;
  } catch (error) {
    result.textContent = `调用失败：${error.message}`;
  } finally {
    callButton.disabled = false;
  }
});
